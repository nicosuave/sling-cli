package elt

import (
	"bufio"
	"context"
	"database/sql"
	"io/ioutil"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/flarco/dbio"

	"github.com/flarco/dbio/connection"

	"github.com/flarco/dbio/filesys"

	"github.com/dustin/go-humanize"
	"github.com/slingdata-io/sling/core/env"

	"github.com/flarco/dbio/database"
	"github.com/flarco/dbio/iop"
	"github.com/flarco/g"
	"github.com/slingdata-io/sling/core/dbt"
	"github.com/spf13/cast"
)

// AllowedProps allowed properties
var AllowedProps = map[string]string{
	"sheet": "Provided for Excel source files. Default is first sheet",
	"range": "Optional for Excel source file. Default is largest table range",
}

var start time.Time
var PermitTableSchemaOptimization = false

// IsStalled determines if the task has stalled (no row increment)
func (t *Task) IsStalled(window float64) bool {
	if strings.Contains(t.Progress, "pre-sql") || strings.Contains(t.Progress, "post-sql") {
		return false
	}
	return time.Since(t.lastIncrement).Seconds() > window
}

// GetCount return the current count of rows processed
func (t *Task) GetCount() (count uint64) {
	if t.StartTime == nil {
		return
	}

	return t.df.Count()
}

// GetRate return the speed of flow (rows / sec)
// secWindow is how many seconds back to measure (0 is since beginning)
func (t *Task) GetRate(secWindow int) (rate int) {
	var secElapsed float64
	if t.StartTime == nil || t.StartTime.IsZero() {
		return
	} else if t.EndTime == nil || t.EndTime.IsZero() {
		st := *t.StartTime
		if secWindow <= 0 {
			secElapsed = time.Since(st).Seconds()
			rate = cast.ToInt(math.Round(cast.ToFloat64(t.df.Count()) / secElapsed))
		} else {
			rate = cast.ToInt(math.Round(cast.ToFloat64((t.df.Count() - t.prevCount) / cast.ToUint64(secWindow))))
			if t.prevCount < t.df.Count() {
				t.lastIncrement = time.Now()
			}
			t.prevCount = t.df.Count()
		}
	} else {
		st := *t.StartTime
		et := *t.EndTime
		secElapsed = cast.ToFloat64(et.UnixNano()-st.UnixNano()) / 1000000000.0
		rate = cast.ToInt(math.Round(cast.ToFloat64(t.df.Count()) / secElapsed))
	}
	return
}

// Execute runs a Sling task.
// This may be a file/db to file/db transfer
func (t *Task) Execute() error {
	env.InitLogger()

	done := make(chan struct{})
	now := time.Now()
	t.StartTime = &now
	t.lastIncrement = now

	if t.Ctx == nil {
		t.Ctx = context.Background()
	}

	go func() {
		defer close(done)
		t.Status = ExecStatusRunning

		if t.Err != nil {
			return
		} else if t.Type == DbSQL {
			t.Err = t.runDbSQL()
		} else if t.Type == DbDbt {
			t.Err = t.runDbDbt()
		} else if t.Type == FileToDB {
			t.Err = t.runFileToDB()
		} else if t.Type == DbToDb {
			t.Err = t.runDbToDb()
		} else if t.Type == DbToFile {
			t.Err = t.runDbToFile()
		} else if t.Type == FileToFile {
			t.Err = t.runFileToFile()
		} else {
			t.SetProgress("task execution configuration is invalid")
			t.Err = g.Error("Cannot Execute. Task Type is not specified")
		}
	}()

	select {
	case <-done:
	case <-t.Ctx.Done():
		// need to add cancelling mechanism here
		// if context is cancelled, need to cleanly cancel task
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		if t.Err == nil {
			t.Err = g.Error("Execution interrupted")
		}
	}

	if t.Err == nil {
		t.SetProgress("execution succeeded")
		t.Status = ExecStatusSuccess
	} else {
		t.SetProgress("execution failed")
		t.Status = ExecStatusError
		t.Err = g.Error(t.Err, "execution failed")
	}

	now2 := time.Now()
	t.EndTime = &now2

	return t.Err
}

func (t *Task) runDbSQL() (err error) {

	start = time.Now()

	t.SetProgress("connecting to target database")
	tgtProps := g.MapToKVArr(t.Cfg.TgtConn.DataS())
	tgtConn, err := database.NewConnContext(t.Ctx, t.Cfg.TgtConn.URL(), tgtProps...)
	if err != nil {
		err = g.Error(err, "Could not initialize target connection")
		return
	}

	err = tgtConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Cfg.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	defer tgtConn.Close()

	t.SetProgress("executing sql on target database")
	result, err := tgtConn.ExecContext(t.Ctx, t.Cfg.Target.Object)
	if err != nil {
		err = g.Error(err, "Could not complete sql execution on %s (%s)", t.Cfg.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	rowAffCnt, err := result.RowsAffected()
	if err == nil {
		t.SetProgress("%d rows affected", rowAffCnt)
	}

	return
}

func (t *Task) runDbDbt() (err error) {
	dbtConfig := g.Marshal(t.Cfg.Target.DbtConfig)
	dbtObj, err := dbt.NewDbt(dbtConfig, t.Cfg.TgtConn.Info().Name)
	if err != nil {
		return g.Error(err, "could not init dbt task")
	}
	if dbtObj.Schema == "" {
		dbtObj.Schema = t.Cfg.TgtConn.DataS()["schema"]
	}

	dbtObj.Session.SetScanner(func(stderr bool, text string) {
		switch stderr {
		case true, false:
			t.SetProgress(text)
		}
	})

	err = dbtObj.Init(t.Cfg.TgtConn)
	if err != nil {
		return g.Error(err, "could not initialize dbt project")
	}

	err = dbtObj.Run()
	if err != nil {
		err = g.Error(err, "could not run dbt task")
	}

	return
}

func (t *Task) runDbToFile() (err error) {

	start = time.Now()
	srcProps := g.MapToKVArr(t.Cfg.SrcConn.DataS())
	srcConn, err := database.NewConnContext(t.Ctx, t.Cfg.SrcConn.URL(), srcProps...)
	if err != nil {
		err = g.Error(err, "Could not initialize source connection")
		return
	}

	t.SetProgress("connecting to source database")
	err = srcConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Cfg.SrcConn.Info().Name, srcConn.GetType())
		return
	}

	defer srcConn.Close()

	t.SetProgress("reading from source database")
	t.df, err = t.ReadFromDB(&t.Cfg, srcConn)
	if err != nil {
		err = g.Error(err, "Could not ReadFromDB")
		return
	}
	defer t.df.Close()

	t.SetProgress("writing to target file system")
	cnt, err := t.WriteToFile(&t.Cfg, t.df)
	if err != nil {
		err = g.Error(err, "Could not WriteToFile")
		return
	}

	t.SetProgress("wrote %d rows [%s r/s]", cnt, getRate(cnt))

	err = t.df.Context.Err()
	return

}

func (t *Task) runFolderToDB() (err error) {
	/*
		This will take a URL as a folder path
		1. list the files/folders in it (not recursive)
		2a. run runFileToDB for each of the files, naming the target table respectively
		2b. OR run runFileToDB for each of the files, to the same target able, assume each file has same structure
		3. keep list of file inserted in Job.Settings (view handleExecutionHeartbeat in server_ws.go).

	*/
	return
}

func (t *Task) runFileToDB() (err error) {

	start = time.Now()

	t.SetProgress("connecting to target database")
	tgtProps := g.MapToKVArr(t.Cfg.TgtConn.DataS())
	tgtConn, err := database.NewConnContext(t.Ctx, t.Cfg.TgtConn.URL(), tgtProps...)
	if err != nil {
		err = g.Error(err, "Could not initialize target connection")
		return
	}

	err = tgtConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Cfg.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	defer tgtConn.Close()

	t.SetProgress("reading from source file system")
	t.df, err = t.ReadFromFile(&t.Cfg)
	if err != nil {
		err = g.Error(err, "could not read from file")
		return
	}
	defer t.df.Close()

	// set schema if needed
	t.Cfg.Target.Object = setSchema(cast.ToString(t.Cfg.Target.Data["schema"]), t.Cfg.Target.Object)
	t.Cfg.Target.Options.TableTmp = setSchema(cast.ToString(t.Cfg.Target.Data["schema"]), t.Cfg.Target.Options.TableTmp)

	t.SetProgress("writing to target database")
	cnt, err := t.WriteToDb(&t.Cfg, t.df, tgtConn)
	if err != nil {
		err = g.Error(err, "could not write to database")
		if t.Cfg.Target.TmpTableCreated {
			// need to drop residue
			tgtConn.DropTable(t.Cfg.Target.Options.TableTmp)
		}
		return
	}

	elapsed := int(time.Since(start).Seconds())
	t.SetProgress("inserted %d rows in %d secs [%s r/s]", cnt, elapsed, getRate(cnt))

	if err != nil {
		err = g.Error(t.df.Err(), "error in transfer")
	}
	return
}

func (t *Task) runFileToFile() (err error) {

	start = time.Now()

	t.SetProgress("reading from source file system")
	t.df, err = t.ReadFromFile(&t.Cfg)
	if err != nil {
		err = g.Error(err, "Could not ReadFromFile")
		return
	}
	defer t.df.Close()

	t.SetProgress("writing to target file system")
	cnt, err := t.WriteToFile(&t.Cfg, t.df)
	if err != nil {
		err = g.Error(err, "Could not WriteToFile")
		return
	}

	t.SetProgress("wrote %d rows [%s r/s]", cnt, getRate(cnt))

	if t.df.Err() != nil {
		err = g.Error(t.df.Err(), "Error in runFileToFile")
	}
	return
}

func (t *Task) runDbToDb() (err error) {
	start = time.Now()
	if t.Cfg.Target.Mode == Mode("") {
		t.Cfg.Target.Mode = AppendMode
	}

	// Initiate connections
	srcProps := g.MapToKVArr(t.Cfg.SrcConn.DataS())
	srcConn, err := database.NewConnContext(t.Ctx, t.Cfg.SrcConn.URL(), srcProps...)
	if err != nil {
		err = g.Error(err, "Could not initialize source connection")
		return
	}

	tgtProps := g.MapToKVArr(t.Cfg.TgtConn.DataS())
	tgtConn, err := database.NewConnContext(t.Ctx, t.Cfg.TgtConn.URL(), tgtProps...)
	if err != nil {
		err = g.Error(err, "Could not initialize target connection")
		return
	}

	t.SetProgress("connecting to source database")
	err = srcConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Cfg.SrcConn.Info().Name, srcConn.GetType())
		return
	}

	t.SetProgress("connecting to target database")
	err = tgtConn.Connect()
	if err != nil {
		err = g.Error(err, "Could not connect to: %s (%s)", t.Cfg.TgtConn.Info().Name, tgtConn.GetType())
		return
	}

	defer srcConn.Close()
	defer tgtConn.Close()

	// set schema if needed
	t.Cfg.Target.Object = setSchema(cast.ToString(t.Cfg.Target.Data["schema"]), t.Cfg.Target.Object)
	t.Cfg.Target.Options.TableTmp = setSchema(cast.ToString(t.Cfg.Target.Data["schema"]), t.Cfg.Target.Options.TableTmp)

	// check if table exists by getting target columns
	t.Cfg.Target.Columns, _ = tgtConn.GetSQLColumns("select * from " + t.Cfg.Target.Object)

	if t.Cfg.Target.Mode == "upsert" {
		t.SetProgress("getting checkpoint value")
		t.Cfg.UpsertVal, err = getUpsertValue(&t.Cfg, tgtConn, srcConn.Template().Variable)
		if err != nil {
			err = g.Error(err, "Could not getUpsertValue")
			return err
		}
	}

	t.SetProgress("reading from source database")
	t.df, err = t.ReadFromDB(&t.Cfg, srcConn)
	if err != nil {
		err = g.Error(err, "Could not ReadFromDB")
		return
	}
	defer t.df.Close()

	// to DirectLoad if possible
	if t.df.FsURL != "" {
		data := g.M("url", t.df.FsURL)
		for k, v := range srcConn.Props() {
			data[k] = v
		}
		t.Cfg.Source.Data["SOURCE_FILE"] = g.M("data", data)
	}

	t.SetProgress("writing to target database")
	cnt, err := t.WriteToDb(&t.Cfg, t.df, tgtConn)
	if err != nil {
		err = g.Error(err, "Could not WriteToDb")
		return
	}

	elapsed := int(time.Since(start).Seconds())
	t.SetProgress("inserted %d rows in %d secs [%s r/s]", cnt, elapsed, getRate(cnt))

	if t.df.Context.Err() != nil {
		err = g.Error(t.df.Context.Err(), "Error running runDbToDb")
	}
	return
}

// ReadFromDB reads from a source database
func (t *Task) ReadFromDB(cfg *Config, srcConn database.Connection) (df *iop.Dataflow, err error) {

	srcTable := ""
	sql := ""
	fieldsStr := "*"
	streamNameArr := regexp.MustCompile(`\s`).Split(cfg.Source.Stream, -1)
	if len(streamNameArr) == 1 { // has no whitespace, is a table/view
		srcTable = streamNameArr[0]
		srcTable = setSchema(cast.ToString(cfg.Source.Data["schema"]), srcTable)
		sql = g.F(`select %s from %s`, fieldsStr, srcTable)
	} else {
		sql = cfg.Source.Stream
	}

	// check if referring to a SQL file
	if strings.HasSuffix(strings.ToLower(cfg.Source.Stream), ".sql") {
		// for upsert, need to put `{upsert_where_cond}` for proper selecting
		sqlFromFile, err := GetSQLText(cfg.Source.Stream)
		if err != nil {
			err = g.Error(err, "Could not get GetSQLText for: "+cfg.Source.Stream)
			if srcTable == "" {
				return df, err
			} else {
				err = nil // don't return error in case the table full name ends with .sql
				g.LogError(err)
			}
		} else {
			sql = sqlFromFile
		}
	}

	if srcTable != "" && cfg.Target.Mode != "drop" && len(t.Cfg.Target.Columns) > 0 {
		// since we are not dropping and table exists, we need to only select the matched columns
		columns, _ := srcConn.GetSQLColumns("select * from " + srcTable)

		if len(columns) > 0 {
			commFields := database.CommonColumns(
				columns.Names(),
				t.Cfg.Target.Columns.Names(),
			)
			if len(commFields) == 0 {
				err = g.Error("src table and tgt table have no columns with same names. Column names must match")
				return
			}
			fieldsStr = strings.Join(commFields, ", ")
		}
	}

	// Get source columns
	cfg.Source.Columns, err = srcConn.GetSQLColumns(g.R(sql, "upsert_where_cond", "1=0"))
	if err != nil {
		err = g.Error(err, "Could not obtain source columns")
		return
	}

	if cfg.Target.Mode == "upsert" {
		// select only records that have been modified after last max value
		upsertWhereCond := "1=1"
		if cfg.UpsertVal != "" {
			upsertWhereCond = g.R(
				"{update_key} >= {value}",
				"update_key", srcConn.Quote(cfg.Source.Columns.Normalize(cfg.Target.UpdateKey)),
				"value", cfg.UpsertVal,
			)
		}

		if srcTable != "" {
			sql = g.R(
				`select {fields} from {table} where {upsert_where_cond}`,
				"fields", fieldsStr,
				"table", srcTable,
				"upsert_where_cond", upsertWhereCond,
			)
		} else {
			if !strings.Contains(sql, "{upsert_where_cond}") {
				err = g.Error("For upsert loading with custom SQL, need to include where clause placeholder {upsert_where_cond}. e.g: select * from my_table where col2='A' AND {upsert_where_cond}")
				return
			}
			sql = g.R(sql, "upsert_where_cond", upsertWhereCond)
		}
	} else if cfg.Source.Limit > 0 && srcTable != "" {
		sql = g.R(
			srcConn.Template().Core["limit"],
			"fields", fieldsStr,
			"table", srcTable,
			"limit", cast.ToString(cfg.Source.Limit),
		)
	}

	df, err = srcConn.BulkExportFlow(sql)
	if err != nil {
		err = g.Error(err, "Could not BulkStream: "+sql)
		return
	}

	return
}

// ReadFromFile reads from a source file
func (t *Task) ReadFromFile(cfg *Config) (df *iop.Dataflow, err error) {

	var stream *iop.Datastream

	if cfg.SrcConn.URL() != "" {
		// construct props by merging with options
		options := g.M()
		g.Unmarshal(g.Marshal(cfg.Source.Options), &options)
		props := append(
			g.MapToKVArr(cfg.SrcConn.DataS()), g.MapToKVArr(g.ToMapString(options))...,
		)

		fs, err := filesys.NewFileSysClientFromURLContext(t.Ctx, cfg.SrcConn.URL(), props...)
		if err != nil {
			err = g.Error(err, "Could not obtain client for: "+cfg.SrcConn.URL())
			return df, err
		}

		df, err = fs.ReadDataflow(cfg.SrcConn.URL())
		if err != nil {
			err = g.Error(err, "Could not FileSysReadDataflow for: "+cfg.SrcConn.URL())
			return df, err
		}
	} else {
		stream, err = filesys.MakeDatastream(bufio.NewReader(os.Stdin))
		if err != nil {
			err = g.Error(err, "Could not MakeDatastream")
			return
		}
		df, err = iop.MakeDataFlow(stream)
		if err != nil {
			err = g.Error(err, "Could not MakeDataFlow for Stdin")
			return
		}
	}

	return
}

// WriteToFile writes to a target file
func (t *Task) WriteToFile(cfg *Config, df *iop.Dataflow) (cnt uint64, err error) {
	var stream *iop.Datastream
	var bw int64

	if cfg.TgtConn.URL() != "" {
		dateMap := iop.GetISO8601DateMap(time.Now())
		cfg.TgtConn.Set(g.M("url", g.Rm(cfg.TgtConn.URL(), dateMap)))

		// construct props by merging with options
		options := g.M()
		g.Unmarshal(g.Marshal(cfg.Target.Options), &options)
		props := append(
			g.MapToKVArr(cfg.TgtConn.DataS()), g.MapToKVArr(g.ToMapString(options))...,
		)

		fs, err := filesys.NewFileSysClientFromURLContext(t.Ctx, cfg.TgtConn.URL(), props...)
		if err != nil {
			err = g.Error(err, "Could not obtain client for: "+cfg.TgtConn.URL())
			return cnt, err
		}

		bw, err = fs.WriteDataflow(df, cfg.TgtConn.URL())
		if err != nil {
			err = g.Error(err, "Could not FileSysWriteDataflow")
			return cnt, err
		}
		cnt = df.Count()
	} else if cfg.Options.StdOut {
		stream = iop.MergeDataflow(df)
		stream.SetConfig(map[string]string{"delimiter": ","})
		reader := stream.NewCsvReader(0, 0)
		bufStdout := bufio.NewWriter(os.Stdout)
		defer bufStdout.Flush()
		bw, err = filesys.Write(reader, bufStdout)
		if err != nil {
			err = g.Error(err, "Could not write to Stdout")
			return
		}
		cnt = stream.Count
	} else {
		err = g.Error("target for output is not specified")
		return
	}

	g.Debug(
		"wrote %s: %d rows [%s r/s]",
		humanize.Bytes(cast.ToUint64(bw)), cnt, getRate(cnt),
	)

	return
}

// WriteToDb writes to a target DB
// create temp table
// load into temp table
// insert / upsert / replace into target table
func (t *Task) WriteToDb(cfg *Config, df *iop.Dataflow, tgtConn database.Connection) (cnt uint64, err error) {
	targetTable := cfg.Target.Object

	// set bulk
	if !cfg.Target.Options.UseBulk {
		tgtConn.SetProp("use_bulk", "false")
		tgtConn.SetProp("allow_bulk_import", "false")
	}

	if cfg.Target.Options.TableTmp == "" {
		cfg.Target.Options.TableTmp = targetTable
		if g.In(tgtConn.GetType(), dbio.TypeDbOracle) && len(cfg.Target.Options.TableTmp) > 24 {
			cfg.Target.Options.TableTmp = cfg.Target.Options.TableTmp[:24] // max is 30 chars
		}
		cfg.Target.Options.TableTmp = cfg.Target.Options.TableTmp + "_tmp" + g.RandString(g.NumericRunes, 1) + strings.ToLower(g.RandString(g.AplhanumericRunes, 1))
	}
	if cfg.Target.Mode == "" {
		cfg.Target.Mode = "append"
	}

	// pre SQL
	if cfg.Target.Options.PreSQL != "" {
		t.SetProgress("executing pre-sql")
		sql, err := GetSQLText(cfg.Target.Options.PreSQL)
		if err != nil {
			err = g.Error(err, "could not get pre-sql body")
			return cnt, err
		}
		_, err = tgtConn.Exec(sql)
		if err != nil {
			err = g.Error(err, "could not execute pre-sql on target")
			return cnt, err
		}
	}

	// Drop & Create the temp table
	err = tgtConn.DropTable(cfg.Target.Options.TableTmp)
	if err != nil {
		err = g.Error(err, "could not drop table "+cfg.Target.Options.TableTmp)
		return
	}
	sampleData := iop.NewDataset(df.Columns)
	sampleData.Rows = df.Buffer
	sampleData.SafeInference = true
	_, err = createTableIfNotExists(tgtConn, sampleData, cfg.Target.Options.TableTmp, "")
	if err != nil {
		err = g.Error(err, "could not create temp table "+cfg.Target.Options.TableTmp)
		return
	}
	cfg.Target.TmpTableCreated = true
	defer tgtConn.DropTable(cfg.Target.Options.TableTmp)

	// TODO: if srcFile is from a cloud storage, and targetDB
	// supports direct loading (RedShift, Snowflake or Azure)
	// do direct loading (without passing through our box)
	// risk is potential data loss, since we cannot validate counts
	srcFile, _ := connection.NewConnectionFromMap(g.M())
	if sf, ok := t.Cfg.Source.Data["SOURCE_FILE"]; ok {
		srcFile, err = connection.NewConnectionFromMap(sf.(g.Map))
		if err != nil {
			err = g.Error(err, "could not create data conn for SOURCE_FILE")
			return
		}
	}

	cnt, ok, err := connection.CopyDirect(tgtConn, cfg.Target.Options.TableTmp, srcFile)
	if ok {
		df.SetEmpty() // this executes deferred functions (such as file residue removal
		if err != nil {
			err = g.Error(err, "could not directly load into database")
			return
		}
		g.Debug("copied directly from cloud storage")
		cnt, _ = tgtConn.GetCount(cfg.Target.Options.TableTmp)
		// perhaps do full analysis to validate quality
	} else {
		t.SetProgress("streaming inserts")
		cnt, err = tgtConn.BulkImportFlow(cfg.Target.Options.TableTmp, df)
		if err != nil {
			err = g.Error(err, "could not insert into "+targetTable)
			return
		}
		tCnt, _ := tgtConn.GetCount(cfg.Target.Options.TableTmp)
		if cnt != tCnt {
			err = g.Error("inserted in temp table but table count (%d) != stream count (%d). Records missing. Aborting", tCnt, cnt)
			return
		}
		// aggregate stats from stream processors
		df.SyncStats()

		// Checksum Comparison, data quality
		err = tgtConn.CompareChecksums(cfg.Target.Options.TableTmp, df.Columns)
		if err != nil {
			g.Debug(err.Error())
		}
	}

	// need to contain the final write in a transcation after data is loaded
	txOptions := sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: false}
	switch tgtConn.GetType() {
	case dbio.TypeDbSnowflake:
		txOptions = sql.TxOptions{}
	}
	err = tgtConn.BeginContext(df.Context.Ctx, &txOptions)
	if err != nil {
		err = g.Error(err, "could not open transcation to write to final table")
		return
	}

	defer tgtConn.Rollback() // rollback in case of error

	if cnt > 0 {
		if cfg.Target.Mode == "drop" {
			// drop, (create if not exists) and insert directly
			err = tgtConn.DropTable(targetTable)
			if err != nil {
				err = g.Error(err, "could not drop table "+targetTable)
				return
			}
			t.SetProgress("dropped table " + targetTable)
		}

		// create table if not exists
		sample := iop.NewDataset(df.Columns)
		sample.Rows = df.Buffer
		sample.Inferred = true // already inferred with SyncStats
		created, err := createTableIfNotExists(
			tgtConn,
			sample,
			targetTable,
			cfg.Target.Options.TableDDL,
		)
		if err != nil {
			err = g.Error(err, "could not create table "+targetTable)
			return cnt, err
		} else if created {
			t.SetProgress("created table %s", targetTable)
		}

		// TODO: put corrective action here, based on StreamProcessor's result
		// --> use StreamStats to generate new DDL, create the target
		// table with the new DDL, then insert into.
		// IF! Target table exists, and the DDL is insufficient, then
		// have setting called PermitTableSchemaOptimization, which
		// allows sling elt to alter the final table to fit the data
		// if table doesn't exists, then easy-peasy, create it.
		// change logic in createTableIfNotExists ot use StreamStats
		// OptimizeTable creates the table if it's missing
		// **Hole in this**: will truncate data points, since it is based
		// only on new data being inserted... would need a complete
		// stats of the target table to properly optimize.
		if !created && PermitTableSchemaOptimization {
			err = tgtConn.OptimizeTable(targetTable, df.Columns)
			if err != nil {
				err = g.Error(err, "could not optimize table schema")
				return cnt, err
			}
		}
	}

	// Put data from tmp to final
	if cnt == 0 {
		t.SetProgress("0 rows inserted. Nothing to do.")
	} else if cfg.Target.Mode == "drop (need to optimize temp table in place)" {
		// use swap
		err = tgtConn.SwapTable(cfg.Target.Options.TableTmp, targetTable)
		if err != nil {
			err = g.Error(err, "could not swap tables %s to %s", cfg.Target.Options.TableTmp, targetTable)
			return 0, err
		}

		err = tgtConn.DropTable(cfg.Target.Options.TableTmp)
		if err != nil {
			err = g.Error(err, "could not drop table "+cfg.Target.Options.TableTmp)
			return
		}
		t.SetProgress("dropped old table of " + targetTable)

	} else if cfg.Target.Mode == "append" || cfg.Target.Mode == "drop" {
		// create if not exists and insert directly
		err = insertFromTemp(cfg, tgtConn)
		if err != nil {
			err = g.Error(err, "Could not insert from temp")
			return 0, err
		}
	} else if cfg.Target.Mode == "truncate" {
		// truncate (create if not exists) and insert directly
		truncSQL := g.R(
			tgtConn.GetTemplateValue("core.truncate_table"),
			"table", targetTable,
		)
		_, err = tgtConn.Exec(truncSQL)
		if err != nil {
			err = g.Error(err, "Could not truncate table: "+targetTable)
			return
		}
		t.SetProgress("truncated table " + targetTable)

		// insert
		err = insertFromTemp(cfg, tgtConn)
		if err != nil {
			err = g.Error(err, "Could not insert from temp")
			// data is still in temp table at this point
			// need to decide whether to drop or keep it for future use
			return 0, err
		}
	} else if cfg.Target.Mode == "upsert" {
		// insert in temp
		// create final if not exists
		// delete from final and insert
		// or update (such as merge or ON CONFLICT)
		rowAffCnt, err := tgtConn.Upsert(cfg.Target.Options.TableTmp, targetTable, cfg.Target.PrimaryKey)
		if err != nil {
			err = g.Error(err, "Could not upsert from temp")
			// data is still in temp table at this point
			// need to decide whether to drop or keep it for future use
			return 0, err
		}
		t.SetProgress("%d INSERTS / UPDATES", rowAffCnt)
	}

	// post SQL
	if postSQL := cfg.Target.Options.PostSQL; postSQL != "" {
		t.SetProgress("executing post-sql")
		if strings.HasSuffix(strings.ToLower(postSQL), ".sql") {
			postSQL, err = GetSQLText(cfg.Target.Options.PostSQL)
			if err != nil {
				err = g.Error(err, "Error executing Target.PostSQL. Could not get GetSQLText for: "+cfg.Target.Options.PostSQL)
				return cnt, err
			}
		}
		_, err = tgtConn.Exec(postSQL)
		if err != nil {
			err = g.Error(err, "Error executing Target.PostSQL")
			return cnt, err
		}
	}

	err = tgtConn.Commit()
	if err != nil {
		err = g.Error(err, "could not commit")
		return cnt, err
	}

	err = df.Context.Err()
	return
}

func createTableIfNotExists(conn database.Connection, data iop.Dataset, tableName string, tableDDL string) (created bool, err error) {

	// check table existence
	exists, err := conn.TableExists(tableName)
	if err != nil {
		return false, g.Error(err, "Error checking table "+tableName)
	} else if exists {
		return false, nil
	}

	if tableDDL == "" {
		tableDDL, err = conn.GenerateDDL(tableName, data, false)
		if err != nil {
			return false, g.Error(err, "Could not generate DDL for "+tableName)
		}
	}

	_, err = conn.Exec(tableDDL)
	if err != nil {
		errorFilterTableExists := conn.GetTemplateValue("variable.error_filter_table_exists")
		if errorFilterTableExists != "" && strings.Contains(err.Error(), errorFilterTableExists) {
			return false, g.Error(err, "Error creating table %s as it already exists", tableName)
		}
		return false, g.Error(err, "Error creating table "+tableName)
	}

	return true, nil
}

func insertFromTemp(cfg *Config, tgtConn database.Connection) (err error) {
	// insert
	tmpColumns, err := tgtConn.GetColumns(cfg.Target.Options.TableTmp)
	if err != nil {
		err = g.Error(err, "could not get column list for "+cfg.Target.Options.TableTmp)
		return
	}
	tgtColumns, err := tgtConn.GetColumns(cfg.Target.Object)
	if err != nil {
		err = g.Error(err, "could not get column list for "+cfg.Target.Object)
		return
	}

	// if tmpColumns are dummy fields, simply match the target column names
	if iop.IsDummy(tmpColumns) && len(tmpColumns) == len(tgtColumns) {
		for i, col := range tgtColumns {
			tmpColumns[i].Name = col.Name
		}
	}

	// TODO: need to validate the source table types are casted
	// into the target column type
	tgtFields, err := tgtConn.ValidateColumnNames(
		tgtColumns.Names(),
		tmpColumns.Names(),
		true,
	)
	if err != nil {
		err = g.Error(err, "columns mismatched")
		return
	}

	srcFields := tgtConn.CastColumnsForSelect(tmpColumns, tgtColumns)

	sql := g.R(
		tgtConn.Template().Core["insert_from_table"],
		"tgt_table", cfg.Target.Object,
		"src_table", cfg.Target.Options.TableTmp,
		"tgt_fields", strings.Join(tgtFields, ", "),
		"src_fields", strings.Join(srcFields, ", "),
	)
	_, err = tgtConn.Exec(sql)
	if err != nil {
		err = g.Error(err, "Could not execute SQL: "+sql)
		return
	}
	g.Debug("inserted rows into `%s` from temp table `%s`", cfg.Target.Object, cfg.Target.Options.TableTmp)
	return
}

func getUpsertValue(cfg *Config, tgtConn database.Connection, srcConnVarMap map[string]string) (val string, err error) {
	// get table columns type for table creation if not exists
	// in order to get max value
	// does table exists?
	// get max value from key_field
	sql := g.F(
		"select max(%s) as max_val from %s",
		cfg.Target.UpdateKey,
		cfg.Target.Object,
	)

	data, err := tgtConn.Query(sql)
	if err != nil {
		if strings.Contains(err.Error(), "exist") {
			// table does not exists, will be create later
			// set val to blank for full load
			return "", nil
		}
		err = g.Error(err, "could not get max value for "+cfg.Target.UpdateKey)
		return
	}
	if len(data.Rows) == 0 {
		// table is empty
		// set val to blank for full load
		return "", nil
	}

	value := data.Rows[0][0]
	colType := data.Columns[0].Type
	if strings.Contains("datetime,date,timestamp", colType) {
		val = g.R(
			srcConnVarMap["timestamp_layout_str"],
			"value", cast.ToTime(value).Format(srcConnVarMap["timestamp_layout"]),
		)
	} else if strings.Contains("datetime,date", colType) {
		val = g.R(
			srcConnVarMap["date_layout_str"],
			"value", cast.ToTime(value).Format(srcConnVarMap["date_layout"]),
		)
	} else if strings.Contains("integer,bigint,decimal", colType) {
		val = cast.ToString(value)
	} else {
		val = strings.ReplaceAll(cast.ToString(value), `'`, `''`)
		val = `'` + val + `'`
	}

	return
}

func getRate(cnt uint64) string {
	return humanize.Commaf(math.Round(cast.ToFloat64(cnt) / time.Since(start).Seconds()))
}

// GetSQLText process source sql file / text
func GetSQLText(sqlStr string) (string, error) {
	sql := sqlStr

	_, err := os.Stat(sqlStr)
	if err == nil {
		bytes, err := ioutil.ReadFile(sqlStr)
		if err != nil {
			return "", g.Error(err, "Could not ReadFile: "+sqlStr)
		}
		sql = string(bytes)
	}

	return sql, nil
}
