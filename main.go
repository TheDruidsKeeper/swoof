package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	mysql "github.com/StirlingMarketingGroup/cool-mysql"
	"github.com/dustin/go-humanize/english"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"github.com/posener/cmd"
	"github.com/vbauerster/mpb/v5"
	"github.com/vbauerster/mpb/v5/decor"
)

var confDir, _ = os.UserConfigDir()

var (
	root = cmd.New()

	aliasesFiles = root.String("a", confDir+"/swoof/aliases.yaml", "your alaises file")

	connectionsFile = root.String("c", confDir+"/swoof/connections.yaml", "your connections file")

	disableTransactions = root.Bool("disable-tx", false, "disables transactions for inserts, this will dramatically slow down imports")

	skipData = root.Bool("n", false, "drop/create tables and triggers only, without importing data")

	threads = root.Int("t", 4, "max concurrent tables at the same time, import stability may vary wildly between servers while increasing this")

	// not entirely sure how much this really affects performance,
	// since the performance bottleneck is almost guaranteed to be writing
	// the rows to the source
	rowBufferSize = root.Int("r", 50, "max rows buffer size. Will have this many rows downloaded and ready for importing")

	tempTablePrefix = root.String("p", "_swoof_", "prefix of the temp table used for initial creation before the swap and drop")

	args = root.Args("source, dest, tables", "source, dest, tables, ex:\n"+
		"swoof [flags] 'user:pass@(host)/dbname' 'user:pass@(host)/dbname' table1 table2 table3\n\n"+
		"see: https://github.com/go-sql-driver/mysql#dsn-data-source-name\n\n"+
		"Or, optionally, you can use your connections in your connections file like so:\n\n"+
		"swoof [flags] production localhost table1 table2 table3")
)

func main() {
	start := time.Now()

	// parse our command line arguments and make sure we
	// were given something that makes sense
	root.ParseArgs(os.Args...)
	if len(*args) < 3 {
		root.Usage()
		os.Exit(1)
	}

	// guard channel of structs makes sure we can easily block for
	// running only a max number of goroutines at a time
	var guard = make(chan struct{}, *threads)

	sourceDSN := (*args)[0]
	destDSN := (*args)[1]

	// lookup connection information in the users config file
	// for much easier and shorter (and probably safer) command usage
	if connections, err := getConnections(*connectionsFile); err == nil {
		if c, ok := connections[sourceDSN]; ok {
			if c.DestOnly {
				panic(errors.Errorf("can't use %q as a source per your config", sourceDSN))
			}

			if c.Params == nil {
				c.Params = make(map[string]string)
			}
			c.Params["parseTime"] = "true"

			sourceDSN = connectionToDSN(c)
		}

		if c, ok := connections[destDSN]; ok {
			if c.SourceOnly {
				panic(errors.Errorf("can't use %q as a destination per your config", destDSN))
			}

			if c.Params == nil {
				c.Params = make(map[string]string)
			}
			c.Params["parseTime"] = "true"

			destDSN = connectionToDSN(c)
		}
	}

	// source connection is the first argument
	// this is where our rows are coming from
	src, err := mysql.NewFromDSN(sourceDSN, sourceDSN)
	if err != nil {
		panic(err)
	}

	src.DisableUnusedColumnWarnings = true

	tableNames, err := getTables(*aliasesFiles, args, src)
	if err != nil {
		panic(err)
	}

	// and now we can get our tables ordered by the largest physical tables first
	// this *should* help performance, so that the longest table doesn't start last
	// and draw out the total process time
	// this also has the nice side effect of de-duplicating our tables list
	tables := make(chan struct {
		TableName string `mysql:"table_name"`
	})
	go func() {
		defer close(tables)
		err = src.Select(tables, "select`table_name`"+
			"from`information_schema`.`TABLES`"+
			"where`table_schema`=database()"+
			"and`table_name`in(@@Tables)"+
			"order by`data_length`+`index_length`desc", 0, mysql.Params{
			"Tables": *tableNames,
		})
		if err != nil {
			panic(err)
		}
	}()

	// our multi-progress bar ties right into our wait group
	var wg sync.WaitGroup
	pb := mpb.New(mpb.WithWaitGroup(&wg))

	tableCount := 0
	for table := range tables {
		tableCount++

		// this makes sure we capture tableName in a way that it doesn't
		// change on us within our loop
		// And IMO this is cleaner than having the func below accept the string
		tableName := table.TableName

		// ensure we only run up to our max imports at a time
		guard <- struct{}{}

		wg.Add(1)

		go func() {
			defer wg.Done()

			// once this function is done, let another task take a place
			// in our guard
			defer func() { <-guard }()

			// the mysql lib we're using, cool mysql, lets us define a
			// chan of structs to read our rows into, for awesome
			// performance and type safety. The tags are the actual column names
			columns := make(chan struct {
				ColumnName           string `mysql:"COLUMN_NAME"`
				Position             int    `mysql:"ORDINAL_POSITION"`
				GenerationExpression string `mysql:"GENERATION_EXPRESSION"`
			})

			// we need to check to see if the db supports generated columns
			// if it doesn't, our query to get column info will fail
			columnInfoCols := "`COLUMN_NAME`,`ORDINAL_POSITION`"
			ok, err := src.Exists("select 0 "+
				"from`information_schema`.`columns`"+
				"where`TABLE_SCHEMA`='INFORMATION_SCHEMA'"+
				"and`table_name`='columns'"+
				"and`column_name`='GENERATION_EXPRESSION'", 0)
			if err != nil {
				panic(err)
			}
			if ok {
				columnInfoCols += ",`GENERATION_EXPRESSION`"
			}

			// in this query we're simply getting all the details about our column names
			// so we can make a dynamic struct that the rows can fit into
			go func() {
				defer close(columns)
				err = src.Select(columns, "select"+columnInfoCols+
					"from`INFORMATION_SCHEMA`.`columns`"+
					"where`TABLE_SCHEMA`=database()"+
					"and`table_name`='"+tableName+"'"+
					"order by`ORDINAL_POSITION`", 0)
				if err != nil {
					panic(err)
				}
			}()

			// this is our string builder for quoted column names,
			// which will be used in our select statement
			columnsQuotedBld := new(strings.Builder)

			i := 0

			// read c from our channel of columns, where c is our column struct
			// cool thing about cool mysql channel selecting, is that the selected row
			// only exists in memory during its loop here, only one at a time
			for c := range columns {
				// you can't insert into generated columns, and mysql will actually
				// throw errors if you try and do this, so we simply skip them altogether
				// and imagine they don't exist
				if len(c.GenerationExpression) != 0 {
					continue
				}

				// column string should be like "`Column1`,`Column2`..."
				if i != 0 {
					columnsQuotedBld.WriteByte(',')
				}
				columnsQuotedBld.WriteByte('`')
				columnsQuotedBld.WriteString(c.ColumnName)
				columnsQuotedBld.WriteByte('`')

				i++
			}

			ch := make(chan mysql.SliceRow, *rowBufferSize)

			columnsQuoted := columnsQuotedBld.String()

			// oh yeah, that's just one query. We don't actually have to chunk this selection
			// because we're dealing with rows as they come in, instead of trying to select them
			// all into memory or something first, which makes this code dramatically simpler
			// and should work with tables of all sizes
			go func() {
				defer close(ch)
				err = src.Select(ch, "select /*+ MAX_EXECUTION_TIME(2147483647) */ "+columnsQuoted+"from`"+tableName+"`", 0)
				if err != nil {
					panic(err)
				}
			}()

			// we make the dest connection in the loop, once per table, because
			// we need to be able to set foreign keys off, and the connection pooling
			// by default makes this difficult. So instead we declare it here, and turn off
			// pooling, almost creating our own "pool"
			dstDB, err := mysql.NewFromDSN(destDSN, destDSN)
			if err != nil {
				panic(err)
			}

			src.DisableUnusedColumnWarnings = true

			type Executor interface {
				Exec(query string, params ...any) error
				I() *mysql.Inserter
			}

			// use a transaction for all the dst commands, if tx is not disabled
			// NOTE: if tx is disabled, then you will almost for sure get foreign key errors
			var dst Executor
			var cancel func() error
			if *disableTransactions {
				dst = dstDB
			} else {
				dst, cancel, err = dstDB.BeginTx()
				defer cancel()
				if err != nil {
					panic(fmt.Errorf("failed to begin transaction: %w", err))
				}
			}

			// we need to disable this because importing two tables at the same time
			// will almost certainly run into problems otherwise. Also dropping the table
			// will do the same
			err = dst.Exec("set`FOREIGN_KEY_CHECKS`=0")
			if err != nil {
				panic(err)
			}

			var count int64
			if !*skipData {
				// and get the count, so we can show are swick progress bars
				err = src.Select(&count, "select count(*)`Count`from`"+tableName+"`", 0)
				if err != nil {
					panic(err)
				}
			}

			// now we get the table creation syntax from our source
			var table struct {
				CreateMySQL string `mysql:"Create Table"`
			}
			err = src.Select(&table, "show create table`"+tableName+"`", 0)
			if err != nil {
				panic(err)
			}

			tempTableName := *tempTablePrefix + tableName

			// delete the table from our destination
			err = dst.Exec("drop table if exists`" + tempTableName + "`")
			if err != nil {
				panic(err)
			}

			// since foreign key constraints have globally unique names (for some reason)
			// we can't just create our temp table with constraints because
			// the names will likely conflict with the table that already exists

			// so we will strip the constraints here and add them back once we're done
			var constraints string

			// we can safely assume the constraints start like this because you can't
			// constraints without columns!
			constraintsStart := strings.Index(table.CreateMySQL, ",\n  CONSTRAINT ")
			if constraintsStart != -1 {
				// we have the start of our constraints block, and since mysql
				// always (hopefully) gives them in a block, we can find the last
				// constraint and everything in the middle is what we want
				constraintsEnd := strings.LastIndex(table.CreateMySQL, ",\n  CONSTRAINT ")

				// but we need the end of the line, so we'll get the byte index of the newline
				// after our last index as our end marker
				constraintsEnd = constraintsEnd + strings.IndexByte(table.CreateMySQL[constraintsEnd+2:], '\n') + 2

				// then we can keep track of our constraints so we can add them back
				// to our table once we've dropped the original table
				constraints = table.CreateMySQL[constraintsStart:constraintsEnd]

				// and store our create query without our constraints
				table.CreateMySQL = table.CreateMySQL[:constraintsStart] + table.CreateMySQL[constraintsEnd:]
			}

			// now we can make the table on our destination
			err = dst.Exec("CREATE TABLE `" + tempTableName + "`" + strings.TrimPrefix(table.CreateMySQL, "CREATE TABLE `"+tableName+"`"))
			if err != nil {
				panic(err)
			}

			// our pretty bar config for the progress bars
			// their documention lives over here https://github.com/vbauerster/mpb
			bar := pb.AddBar(count,
				mpb.BarStyle("|▇▇ |"),
				mpb.PrependDecorators(
					decor.Name(color.HiBlueString(tableName)),
					decor.OnComplete(decor.Percentage(decor.WC{W: 5}), color.HiMagentaString(" done!")),
				),
				mpb.AppendDecorators(
					decor.CountersNoUnit("( "+color.HiCyanString("%d/%d")+", ", decor.WCSyncWidth),
					decor.AverageSpeed(-1, " "+color.HiGreenString("%.2f/s")+" ) ", decor.WCSyncWidth),
					decor.AverageETA(decor.ET_STYLE_MMSS),
				),
			)

			if !*skipData {
				// and if we aren't skipping the data, start the import!
				// Now this *does* have to be chunked because there's no way to stream
				// rows to mysql, but cool mysql handles this for us, all it needs is the same
				// channel we got from the select
				err = dst.I().SetAfterRowExec(func(start time.Time) {
					bar.Increment()
					bar.DecoratorEwmaUpdate(time.Since(start))
				}).Insert("insert into`"+tempTableName+"`("+columnsQuoted+")", ch)
				if err != nil {
					panic(err)
				}
			}

			// and just in case the rows have changed count since our count selection,
			// we'll just tell the progress bar that we're finished
			bar.SetTotal(bar.Current(), true)

			// drop the old table now that our temp table is done
			err = dst.Exec("drop table if exists`" + tableName + "`")
			if err != nil {
				panic(err)
			}

			// rename our temp table to the real table name
			// we could do an atomic rename here, but the problem is that atomic renames
			// also rename all the constraints of other tables pointing to our original table, and
			// we want those constraints to point to our new table instead

			// if you're doing this live, there *is* some down time, but other tools handle this the same
			// way, so I don't think it's unreasonable if we do the same
			err = dst.Exec("alter table`" + tempTableName + "`rename`" + tableName + "`")
			if err != nil {
				panic(err)
			}

			// no we can add back our constraints if we have them
			// converting our constraints to alter table syntax by removing our leading
			// comma and adding the word "add" at the beginning of each line
			if len(constraints) != 0 {
				err = dst.Exec("alter table`" + tableName + "`" + strings.ReplaceAll(strings.TrimLeft(constraints, ","), "\n", "\nadd"))
				if err != nil {
					panic(err)
				}
			}

			// but we can't forget our triggers!
			// lets grab the triggers from the source table and make sure
			// we re-create them all on our destination
			triggers := make(chan struct {
				Trigger string
			})
			go func() {
				defer close(triggers)
				err = src.Select(triggers, "show triggers where`table`='"+tableName+"'", 0)
				if err != nil {
					panic(err)
				}
			}()
			for r := range triggers {
				var trigger struct {
					CreateMySQL string `mysql:"SQL Original Statement"`
				}
				err := src.Select(&trigger, "show create trigger`"+r.Trigger+"`", 0)
				if err != nil {
					panic(err)
				}

				err = dst.Exec(trigger.CreateMySQL)
				if err != nil {
					panic(err)
				}
			}

			if tx, ok := dst.(*mysql.Tx); ok {
				err = tx.Commit()
				if err != nil {
					panic(fmt.Errorf("failed to commit transaction: %w", err))
				}
			}
		}()
	}

	pb.Wait()

	fmt.Println("finished importing", tableCount, english.PluralWord(tableCount, "table", ""), "in", time.Since(start))
}
