package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/slingdata-io/sling-cli/core"
	"github.com/slingdata-io/sling-cli/core/dbio"
	"github.com/slingdata-io/sling-cli/core/env"
	"github.com/spf13/cast"
	"syreclabs.com/go/faker"

	"github.com/slingdata-io/sling-cli/core/sling"

	"github.com/flarco/g"
	"github.com/flarco/g/csv"
	"github.com/flarco/g/net"
	"github.com/slingdata-io/sling-cli/core/dbio/connection"
	"github.com/slingdata-io/sling-cli/core/dbio/database"
	d "github.com/slingdata-io/sling-cli/core/dbio/database"
	"github.com/slingdata-io/sling-cli/core/dbio/iop"
	"github.com/stretchr/testify/assert"
)

var testMux sync.Mutex

var ef = env.LoadSlingEnvFile()
var ec = connection.EnvConns{EnvFile: &ef}

type testDB struct {
	name string
	conn d.Connection
}

type connTest struct {
	name      string
	schema    string
	root      string
	useBulk   *bool
	adjustCol *bool
}

var connMap = map[dbio.Type]connTest{
	dbio.TypeDbAzure:             {name: "azuresql"},
	dbio.TypeDbAzureDWH:          {name: "azuredwh"},
	dbio.TypeDbBigQuery:          {name: "bigquery"},
	dbio.TypeDbBigTable:          {name: "bigtable"},
	dbio.TypeDbClickhouse:        {name: "clickhouse", schema: "default", useBulk: g.Bool(true)},
	dbio.Type("clickhouse_http"): {name: "clickhouse_http", schema: "default", useBulk: g.Bool(true)},
	dbio.TypeDbDuckDb:            {name: "duckdb"},
	dbio.TypeDbMariaDB:           {name: "mariadb", schema: "mariadb"},
	dbio.TypeDbMotherDuck:        {name: "motherduck"},
	dbio.TypeDbMySQL:             {name: "mysql", schema: "mysql"},
	dbio.TypeDbOracle:            {name: "oracle", schema: "system"},
	dbio.TypeDbPostgres:          {name: "postgres"},
	dbio.TypeDbRedshift:          {name: "redshift"},
	dbio.TypeDbSnowflake:         {name: "snowflake"},
	dbio.TypeDbSQLite:            {name: "sqlite", schema: "main"},
	dbio.TypeDbSQLServer:         {name: "mssql", schema: "dbo", useBulk: g.Bool(false)},
	dbio.TypeDbStarRocks:         {name: "starrocks"},
	dbio.TypeDbTrino:             {name: "trino", adjustCol: g.Bool(false)},
	dbio.TypeDbMongoDB:           {name: "mongo", schema: "default"},

	dbio.TypeFileLocal:  {name: "local"},
	dbio.TypeFileSftp:   {name: "sftp"},
	dbio.TypeFileAzure:  {name: "azure_storage"},
	dbio.TypeFileS3:     {name: "aws_s3"},
	dbio.TypeFileGoogle: {name: "google_storage"},
	dbio.TypeFileFtp:    {name: "ftp_test_url"},
}

func init() {
	env.InitLogger()
}

func TestOptions(t *testing.T) {
	testCases := []struct {
		input       string
		shouldError bool
		expected    map[string]any
	}{
		{
			input:       ``,
			shouldError: false,
		},
		{
			input:       `.`,
			shouldError: true,
		},
		{
			input:    `{"use_bulk":false}`,
			expected: map[string]any{"use_bulk": false},
		},
		{
			input:    `{"use_bulk": false}`,
			expected: map[string]any{"use_bulk": false},
		},
		{
			input:    `{use_bulk: false}`,
			expected: map[string]any{"use_bulk": false},
		},
		{
			input:    `use_bulk: false`,
			expected: map[string]any{"use_bulk": false},
		},
		{
			input:       `{use_bulk:false}`,
			shouldError: true,
		},
		{
			input:       `use_bulk:false`,
			shouldError: true,
		},
	}

	for _, testCase := range testCases {
		options, err := parsePayload(testCase.input, true)
		msg := g.F("with input: `%s`", testCase.input)
		if testCase.shouldError {
			assert.Error(t, err, msg)
		} else {
			assert.NoError(t, err, msg)
			if testCase.expected != nil {
				assert.Equal(t, testCase.expected, options, msg)
			}
		}
	}
}

func TestCfgPath(t *testing.T) {

	testCfg := func(path string) (err error) {
		cfg, err := sling.NewConfig(path)
		if !g.AssertNoError(t, err) {
			return
		}

		assert.EqualValues(t, "testing", cfg.SrcConn.Info().Name)
		assert.EqualValues(t, "testing", cfg.Source.Stream)
		assert.EqualValues(t, "testing", cfg.SrcConn.URL())
		assert.EqualValues(t, "testing", cfg.TgtConn.Info().Name)
		assert.EqualValues(t, "testing", cfg.Target.Object)
		assert.EqualValues(t, "testing", cfg.TgtConn.URL())
		assert.EqualValues(t, "testing", cfg.Mode)
		assert.EqualValues(t, 111, cfg.Source.Limit())
		assert.EqualValues(t, "testing", cfg.Target.Options.TableDDL)
		assert.EqualValues(t, "testing", cfg.Target.Options.TableTmp)
		assert.EqualValues(t, "testing", cfg.Target.Options.PostSQL)

		return err
	}

	err := testCfg("tests/test1.yaml")
	g.AssertNoError(t, err)

	err = testCfg("tests/test1.json")
	g.AssertNoError(t, err)
}

func TestExtract(t *testing.T) {
	core.Version = "v1.0.43"

	checkUpdate(true)
	assert.NotEmpty(t, updateVersion)

	printUpdateAvailable()

	err := ExtractTarGz(g.UserHomeDir()+"/Downloads/sling/sling_1.0.44_darwin_all.tar.gz", g.UserHomeDir()+"/Downloads/sling")
	g.AssertNoError(t, err)
}

func testSuite(t *testing.T, connType dbio.Type, testNumbers ...int) {
	conn, ok := connMap[connType]
	if !assert.True(t, ok) {
		return
	}

	templateFilePath := "tests/suite.db.template.tsv"
	if connType.IsFile() {
		templateFilePath = "tests/suite.file.template.tsv"
	}

	tempFilePath := g.F("/tmp/tests.%s.tsv", connType.String())
	folderPath := g.F("tests/suite/%s", connType.String())
	testSchema := lo.Ternary(conn.schema == "", "sling_test", conn.schema)
	testTable := g.F("test1k_%s", connType.String())
	testWideFilePath, err := generateLargeDataset(300, 100, false)
	g.LogFatal(err)

	// generate files
	os.RemoveAll(folderPath) // clean up
	os.MkdirAll(folderPath, 0777)

	// replace vars
	filebytes, err := os.ReadFile(templateFilePath)
	g.LogFatal(err)

	fileContent := string(filebytes)
	fileContent = strings.ReplaceAll(fileContent, "[conn]", conn.name)
	fileContent = strings.ReplaceAll(fileContent, "[schema]", testSchema)
	fileContent = strings.ReplaceAll(fileContent, "[folder]", testSchema)
	fileContent = strings.ReplaceAll(fileContent, "[table]", testTable)
	fileContent = strings.ReplaceAll(fileContent, "[wide_file_path]", testWideFilePath)

	err = os.WriteFile(tempFilePath, []byte(fileContent), 0777)
	g.LogFatal(err)

	data, err := iop.ReadCsv(tempFilePath)
	if !g.AssertNoError(t, err) {
		return
	}

	// rewrite correctly for displaying in Github
	testMux.Lock()
	dataT, err := iop.ReadCsv(templateFilePath)
	if !g.AssertNoError(t, err) {
		return
	}
	c := iop.CSV{Path: templateFilePath, Delimiter: '\t'}
	c.WriteStream(dataT.Stream())
	testMux.Unlock()

	for i, rec := range data.Records() {
		options, _ := g.UnmarshalMap(cast.ToString(rec["options"]))
		sourceOptions, _ := g.UnmarshalMap(cast.ToString(rec["source_options"]))
		targetOptions, _ := g.UnmarshalMap(cast.ToString(rec["target_options"]))
		env, _ := g.UnmarshalMap(cast.ToString(rec["env"]))
		primaryKey := []string{}
		if val := cast.ToString(rec["source_primary_key"]); val != "" {
			primaryKey = strings.Split(val, ",")
		}

		if conn.useBulk != nil {
			targetOptions["use_bulk"] = *conn.useBulk
		}
		if conn.adjustCol != nil {
			targetOptions["adjust_column_type"] = *conn.adjustCol
			sourceOptions["columns"] = g.M("code", "decimal")
		}

		task := g.M(
			"source", g.M(
				"conn", cast.ToString(rec["source_conn"]),
				"stream", cast.ToString(rec["source_stream"]),
				"primary_key", primaryKey,
				"update_key", cast.ToString(rec["source_update_key"]),
				"options", sourceOptions,
			),
			"target", g.M(
				"conn", cast.ToString(rec["target_conn"]),
				"object", cast.ToString(rec["target_object"]),
				"options", targetOptions,
			),
			"mode", cast.ToString(rec["mode"]),
			"options", options,
			"env", env,
		)
		taskPath := filepath.Join(folderPath, g.F("%02d.%s.json", i+1, rec["test_name"]))
		taskBytes := []byte(g.Pretty(task))
		err = os.WriteFile(taskPath, taskBytes, 0777)
		g.AssertNoError(t, err)
	}

	time.Sleep(500 * time.Millisecond)
	files, _ := g.ListDir(folderPath)

	if tns := os.Getenv("TESTS"); tns != "" {
		for _, tn := range strings.Split(tns, ",") {
			if strings.HasSuffix(tn, "+") {
				start := cast.ToInt(strings.TrimSuffix(tn, "+"))

				for i := range files {
					if i+1 < start {
						continue
					}
					testNumbers = append(testNumbers, i+1)
				}
			} else if parts := strings.Split(tn, "-"); len(parts) == 2 {
				start := cast.ToInt(parts[0])
				end := cast.ToInt(parts[1])
				for testNumber := start; testNumber <= end; testNumber++ {
					testNumbers = append(testNumbers, testNumber)
				}
			} else if testNumber := cast.ToInt(tn); testNumber > 0 {
				testNumbers = append(testNumbers, testNumber)
			}
		}
	}

	for i, file := range files {
		if len(testNumbers) > 0 && !g.In(i+1, testNumbers...) {
			continue
		}
		runOneTask(t, file, connType)
		if t.Failed() {
			g.LogError(g.Error("Test `%s` Failed for => %s", file.Name, connType))
			return
		}
	}
}

func runOneTask(t *testing.T, file g.FileItem, dbType dbio.Type) {
	os.Setenv("ERROR_ON_CHECKSUM_FAILURE", "1") // so that it errors when checksums don't match
	println()

	bars := "---------------------------"
	g.Info("%s Testing %s (%s) %s", bars, file.RelPath, dbType, bars)

	cfg := &sling.Config{}
	err := cfg.Unmarshal(file.FullPath)
	if !g.AssertNoError(t, err) {
		return
	}

	if string(cfg.Mode) == "discover" {
		testDiscover(t, cfg, dbType)
		return
	}

	task := sling.NewTask("", cfg)
	if !g.AssertNoError(t, task.Err) {
		return
	}

	// validate object name
	if name, ok := task.Config.Env["validation_object"]; ok {
		if !assert.Equal(t, name, task.Config.Target.Object) {
			return
		}
	}

	if doDelete := task.Config.Env["delete_duck_db"]; cast.ToBool(doDelete) {
		os.Remove(strings.TrimPrefix(task.Config.TgtConn.URL(), "duckdb://"))
	}

	// process PostSQL for different drop_view syntax
	dbConn, err := task.Config.TgtConn.AsDatabase()
	if err == nil {
		table, _ := database.ParseTableName(task.Config.Target.Object, dbConn.GetType())
		table.Name = strings.TrimSuffix(table.Name, "_pg") + "_vw"
		if dbConn.GetType().DBNameUpperCase() {
			table.Name = strings.ToUpper(table.Name)
		}
		viewName := table.FullName()
		dropViewSQL := g.R(dbConn.GetTemplateValue("core.drop_view"), "view", viewName)
		dropViewSQL = strings.TrimSpace(dropViewSQL)
		task.Config.Target.Options.PreSQL = g.R(
			task.Config.Target.Options.PreSQL,
			"drop_view", dropViewSQL,
		)
		task.Config.Target.Options.PostSQL = g.R(
			task.Config.Target.Options.PostSQL,
			"drop_view", dropViewSQL,
		)
	}

	if g.AssertNoError(t, task.Err) {
		err = task.Execute()
		if !g.AssertNoError(t, err) {
			return
		}
	} else {
		return
	}

	// validate count
	if task.Config.Mode == sling.FullRefreshMode && task.Config.TgtConn.Type.IsDb() {
		g.Debug("getting count for test validation")
		conn, err := task.Config.TgtConn.AsDatabase()
		if g.AssertNoError(t, err) {
			count, err := conn.GetCount(task.Config.Target.Object)
			g.AssertNoError(t, err)
			assert.EqualValues(t, task.GetCount(), count)
			conn.Close()
			if valRowCount := cast.ToInt(cfg.Env["validation_row_count"]); valRowCount > 0 {
				assert.EqualValues(t, valRowCount, count)
			}
		}
	}

	// validate file
	if val, ok := task.Config.Env["validation_file"]; ok {
		valFile := strings.TrimPrefix(val, "file://")
		dataFile, err := iop.ReadCsv(valFile)
		if !g.AssertNoError(t, err) {
			return
		}
		orderByStr := "1"
		if len(task.Config.Source.PrimaryKey()) > 0 {
			orderByStr = strings.Join(task.Config.Source.PrimaryKey(), ", ")
		}
		sql := g.F("select * from %s order by %s", task.Config.Target.Object, orderByStr)
		conn, _ := task.Config.TgtConn.AsDatabase()
		dataDB, err := conn.Query(sql)
		g.AssertNoError(t, err)
		conn.Close()
		valCols := strings.Split(task.Config.Env["validation_cols"], ",")

		if g.AssertNoError(t, err) {
			for _, valColS := range valCols {
				valCol := cast.ToInt(valColS)
				valuesFile := dataFile.ColValues(valCol)
				valuesDb := dataDB.ColValues(valCol)
				// g.P(dataDB.ColValues(0))
				// g.P(dataDB.ColValues(1))
				// g.P(valuesDb)
				if assert.Equal(t, len(valuesFile), len(valuesDb)) {
					for i := range valuesDb {
						valDb := dataDB.Sp.ParseString(cast.ToString(valuesDb[i]))
						valFile := dataDB.Sp.ParseString(cast.ToString(valuesFile[i]))
						msg := g.F("row %d, col %d (%s vs %s), in test %s => %#v vs %#v", i+1, valCol, dataFile.Columns[valCol].Name, dataDB.Columns[valCol].Name, file.Name, valDb, valFile)
						if !assert.EqualValues(t, valFile, valDb, msg) {
							g.Warn(msg)
							return
						}
					}
				}
			}
		}
	}
}

func TestSuitePostgres(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbPostgres)
}

// func TestSuiteRedshift(t *testing.T) {
// 	t.Parallel()
// 	testSuite(t, dbio.TypeDbRedshift)
// }

func TestSuiteStarRocks(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbStarRocks)
}

func TestSuiteMySQL(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbMySQL)
}

func TestSuiteMariaDB(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbMariaDB)
}

func TestSuiteOracle(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbOracle)
}

// func TestSuiteBigTable(t *testing.T) {
// 	t.Parallel()
// 	testSuite(t, dbio.TypeDbBigTable)
// }

func TestSuiteBigQuery(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbBigQuery)
}

func TestSuiteSnowflake(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbSnowflake)
}

func TestSuiteSQLite(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbSQLite)
}

func TestSuiteDuckDb(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbDuckDb)
}

// func TestSuiteMotherDuck(t *testing.T) {
// 	t.Parallel()
// 	testSuite(t, dbio.TypeDbMotherDuck)
// }

func TestSuiteSQLServer(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbSQLServer)
}

// func TestSuiteAzure(t *testing.T) {
// 	t.Parallel()
// 	testSuite(t, dbio.TypeDbAzure)
// }

// func TestSuiteAzureDWH(t *testing.T) {
// 	t.Parallel()
// 	testSuite(t, dbio.TypeDbAzureDWH)
// }

func TestSuiteClickhouse(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbClickhouse)
	testSuite(t, dbio.Type("clickhouse_http"))
}

func TestSuiteTrino(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbTrino, 1, 9, 15)
}

func TestSuiteMongo(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeDbMongoDB, 9)
}

// generate large dataset or use cache
func generateLargeDataset(numCols, numRows int, force bool) (path string, err error) {
	testMux.Lock()
	defer testMux.Unlock()

	var data iop.Dataset

	path = g.F("/tmp/dataset_%dx%d.csv", numRows, numCols)
	if _, err = os.Stat(path); err == nil && !force {
		return
	}

	g.Info("generating %s", path)

	type FakeField struct {
		name string
		gen  func() interface{}
	}

	words := make([]string, 100)
	for i := range words {
		words[i] = g.RandString(g.AlphaRunes, g.RandInt(15))
		if len(words[i]) == 10 {
			words[i] = words[i] + "\n"
		}
	}

	fieldsFuncTemplate := []*FakeField{
		{"name", func() interface{} { return faker.Name().Name() }},
		{"url", func() interface{} { return faker.Internet().Url() }},
		{"date_time", func() interface{} { return faker.Date().Forward(100 * time.Minute).Format("2006-01-02 15:04:05") }},
		{"address", func() interface{} { return faker.Address().SecondaryAddress() }},
		{"price", func() interface{} { return faker.Commerce().Price() }},
		{"my_int", func() interface{} { return faker.Number().NumberInt64(5) }},
		{"email", func() interface{} { return faker.Internet().Email() }},
		{"creditcardexpirydate", func() interface{} { return faker.Date().Forward(1000000 * time.Minute).Format("2006-01-02") }},
		{"latitude", func() interface{} { return faker.Address().Latitude() }},
		{"longitude", func() interface{} { return faker.Address().Longitude() }},
		{"interested", func() interface{} { return g.RandInt(2) == 1 }},
		{"paragraph", func() interface{} { return strings.Join(words[:g.RandInt(100)], " ") }},
	}

	fieldsFunc := make([]*FakeField, numCols)
	for i := range fieldsFunc {
		fieldsFunc[i] = fieldsFuncTemplate[i%len(fieldsFuncTemplate)]
	}

	makeRow := func() (row []interface{}) {
		row = make([]interface{}, len(fieldsFunc))
		c := 0
		for _, ff := range fieldsFunc {
			row[c] = ff.gen()
			c++
		}

		return row
	}

	getFields := func() (fields []string) {
		fields = make([]string, len(fieldsFunc))
		for j, ff := range fieldsFunc {
			fields[j] = g.F("%s_%03d", (*ff).name, j+1)
		}
		return
	}

	data = iop.NewDataset(nil)
	data.Rows = make([][]interface{}, numRows)
	data.SetFields(getFields())

	for i := 0; i < 50; i++ {
		data.Rows[i] = makeRow()
	}

	file, _ := os.Create(path)

	w := csv.NewWriter(file)
	defer w.Flush()

	tbw, err := w.Write(append([]string{"id"}, data.GetFields()...))
	if err != nil {
		return path, g.Error(err, "error write row to csv file")
	}

	nullCycle := 0
	for i := 0; i < numRows; i++ {
		rec := make([]string, numCols+1)
		rec[0] = cast.ToString(i + 1) // id
		for j := 1; j < numCols+1; j++ {
			nullCycle++
			if nullCycle%100 == 0 {
				rec[j] = ""
			} else {
				val := data.Rows[i%50][j-1]
				rec[j] = data.Sp.CastToString(j, val, data.Columns[j].Type)
			}
		}

		bw, err := w.Write(rec)
		if err != nil {
			return path, g.Error(err, "error write row to csv file")
		}
		tbw = tbw + bw

		if i%100000 == 0 {
			print(g.F("%d ", i))
		}
	}

	println()

	return path, nil

}

func TestGenerateWideFile(t *testing.T) {
	_, err := generateLargeDataset(300, 100, true)
	g.LogFatal(err)
}

func Test1Replication(t *testing.T) {
	sling.ShowProgress = false
	os.Setenv("DEBUG", "LOW")
	os.Setenv("SLING_CLI", "TRUE")
	os.Setenv("SLING_LOADED_AT_COLUMN", "TRUE")
	os.Setenv("CONCURENCY_LIMIT", "2")
	replicationCfgPath := "tests/replications/r.14.yaml"
	err := runReplication(replicationCfgPath, nil)
	if g.AssertNoError(t, err) {
		return
	}
}

func Test1Task(t *testing.T) {
	os.Setenv("SLING_CLI", "TRUE")
	config := &sling.Config{}
	cfgStr := `
source:
  conn: duckdb
  stream: main.call_center
target:
  conn: starrocks
  object: public.call_center
mode: full-refresh
`
	err := config.Unmarshal(cfgStr)
	if g.AssertNoError(t, err) {
		err = config.Prepare()
		if g.AssertNoError(t, err) {

			task := sling.NewTask("", config)
			g.AssertNoError(t, task.Err)

			// run task
			err = task.Execute()
			g.AssertNoError(t, err)
		}
	}
}

func testDiscover(t *testing.T, cfg *sling.Config, connType dbio.Type) {

	conn := connMap[connType]

	opt := connection.DiscoverOptions{
		Filter:      cfg.Env["filter"],
		ColumnLevel: cast.ToBool(cfg.Env["column_level"]),
		Recursive:   cast.ToBool(cfg.Env["recursive"]),
	}

	if connType.IsDb() && cfg.Target.Object != "" {
		table, err := database.ParseTableName(cfg.Target.Object, connType)
		if !g.AssertNoError(t, err) {
			return
		}

		if table.Name == "*" {
			opt.Schema = table.Schema
		} else {
			opt.Stream = table.FullName()
		}
	} else if connType.IsFile() && cfg.Target.Object != "" {
		if strings.HasSuffix(cfg.Target.Object, "/") {
			opt.Folder = cfg.Target.Object
		} else {
			opt.Stream = cfg.Target.Object
		}
	}

	g.Info("sling conns discover %s %s", conn.name, g.Marshal(opt))
	files, schemata, err := ec.Discover(conn.name, opt)
	if !g.AssertNoError(t, err) {
		return
	}

	valContains := strings.Split(cast.ToString(cfg.Env["validation_contains"]), ",")
	valNotContains := strings.Split(cast.ToString(cfg.Env["validation_not_contains"]), ",")
	valContains = lo.Filter(valContains, func(v string, i int) bool { return v != "" })
	valNotContains = lo.Filter(valNotContains, func(v string, i int) bool { return v != "" })

	valRowCount := cast.ToInt(cfg.Env["validation_row_count"])
	valRowCountMin := -1
	if val := cast.ToString(cfg.Env["validation_row_count"]); strings.HasPrefix(val, ">") {
		valRowCountMin = cast.ToInt(strings.TrimPrefix(val, ">"))
	}

	containsMap := map[string]bool{}
	for _, word := range append(valContains, valNotContains...) {
		if word == "" {
			continue
		}
		containsMap[word] = false
	}

	if connType.IsDb() {
		tables := lo.Values(schemata.Tables())
		columns := iop.Columns(lo.Values(schemata.Columns()))
		if valRowCount > 0 {
			if opt.Stream != "" {
				assert.Equal(t, valRowCount, len(columns))
			} else {
				assert.Equal(t, valRowCount, len(tables))
			}
		} else {
			assert.Greater(t, len(tables), 0)
		}

		if valRowCountMin > -1 {
			if opt.ColumnLevel {
				assert.Greater(t, len(columns), valRowCountMin)
			} else {
				assert.Greater(t, len(tables), valRowCountMin)
			}
		}

		if len(containsMap) > 0 {
			resultType := "tables"

			if opt.ColumnLevel {
				resultType = "columns"
				for _, col := range columns {
					for word := range containsMap {
						if strings.EqualFold(word, col.Name) {
							containsMap[word] = true
						}
					}
				}
			} else {
				for _, table := range tables {
					for word := range containsMap {
						if strings.EqualFold(word, table.Name) {
							containsMap[word] = true
						}
					}
				}
			}

			for _, word := range valContains {
				found := containsMap[word]
				assert.True(t, found, "did not find '%s' in %s for %s", word, resultType, connType)
			}

			for _, word := range valNotContains {
				found := containsMap[word]
				assert.False(t, found, "found '%s' in %s for %s", word, resultType, connType)
			}
		}
	}

	if connType.IsFile() {
		g.Warn(g.Marshal(files.Paths()))
		// basic tests
		assert.Greater(t, len(files), 0)
		for _, uri := range files.URIs() {
			u, err := net.NewURL(uri)
			if !g.AssertNoError(t, err) {
				break
			}

			if !assert.NotContains(t, strings.TrimPrefix(uri, u.U.Scheme+"://"), "//") {
				break
			}
		}

		if len(files) == 0 {
			return
		}

		columns := iop.Columns(lo.Values(files.Columns()))

		if valRowCount > 0 {
			if opt.ColumnLevel {
				assert.Equal(t, valRowCount, len(files[0].Columns))
			} else {
				assert.Equal(t, valRowCount, len(files))
			}
		}

		if valRowCountMin > -1 {
			if opt.ColumnLevel {
				assert.Greater(t, len(files[0].Columns), valRowCountMin)
			} else {
				assert.Greater(t, len(files), valRowCountMin)
			}
		}

		if len(containsMap) > 0 {
			resultType := "files"

			if opt.ColumnLevel {
				resultType = "columns"
				for _, col := range columns {
					for word := range containsMap {
						if strings.EqualFold(word, col.Name) {
							containsMap[word] = true
						}
					}
				}
			} else {
				for _, file := range files {
					for word := range containsMap {
						if strings.Contains(file.URI, word) {
							containsMap[word] = true
						}
					}
				}
			}

			for _, word := range valContains {
				found := containsMap[word]
				assert.True(t, found, "did not find '%s' in %s for %s", word, resultType, connType)
			}

			for _, word := range valNotContains {
				found := containsMap[word]
				assert.False(t, found, "found '%s' in %s for %s", word, resultType, connType)
			}
		}
	}

	g.Info("valContains = %#v", valContains)
	g.Info("valNotContains = %#v", valNotContains)
	g.Info("containsMap = %#v", containsMap)
}

func TestSuiteSftp(t *testing.T) {
	t.Parallel()
	testDiscover(t, nil, dbio.TypeFileSftp)
}

func TestSuiteFileS3(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeFileS3)
}

func TestSuiteFileGoogle(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeFileGoogle)
}

func TestSuiteFileAzure(t *testing.T) {
	t.Parallel()
	testSuite(t, dbio.TypeFileAzure)
}
