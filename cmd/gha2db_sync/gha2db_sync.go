package main

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	lib "github.com/cncf/devstatscode"
	yaml "gopkg.in/yaml.v2"
)

// metrics contain list of metrics to evaluate
type metrics struct {
	Metrics []metric `yaml:"metrics"`
}

// metric contain each metric data
type metric struct {
	Name              string            `yaml:"name"`
	Periods           string            `yaml:"periods"`
	SeriesNameOrFunc  string            `yaml:"series_name_or_func"`
	MetricSQL         string            `yaml:"sql"`
	MetricSQLs        *[]string         `yaml:"sqls"`
	AddPeriodToName   bool              `yaml:"add_period_to_name"`
	Histogram         bool              `yaml:"histogram"`
	Aggregate         string            `yaml:"aggregate"`
	Skip              string            `yaml:"skip"`
	Desc              string            `yaml:"desc"`
	MultiValue        bool              `yaml:"multi_value"`
	EscapeValueName   bool              `yaml:"escape_value_name"`
	AnnotationsRanges bool              `yaml:"annotations_ranges"`
	MergeSeries       string            `yaml:"merge_series"`
	CustomData        bool              `yaml:"custom_data"`
	StartFrom         *time.Time        `yaml:"start_from"`
	LastHours         int               `yaml:"last_hours"`
	SeriesNameMap     map[string]string `yaml:"series_name_map"`
	EnvMap            map[string]string `yaml:"env"`
	Disabled          bool              `yaml:"disabled"`
	Drop              string            `yaml:"drop"`
	Project           string            `yaml:"project"`
}

// randomize - shufflues array of metrics to calculate, making sure that ctx.LastSeries is still last
func (m *metrics) randomize(ctx *lib.Ctx) {
	lib.Printf("Randomizing metrics calculation order\n")
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(m.Metrics), func(i, j int) { m.Metrics[i], m.Metrics[j] = m.Metrics[j], m.Metrics[i] })
	idx := -1
	lastI := len(m.Metrics) - 1
	for i, m := range m.Metrics {
		if m.SeriesNameOrFunc == ctx.LastSeries {
			idx = i
			break
		}
	}
	if idx >= 0 && idx != lastI {
		m.Metrics[idx], m.Metrics[lastI] = m.Metrics[lastI], m.Metrics[idx]
	}
}

// Add _period to all array items
func addPeriodSuffix(seriesArr []string, period string) (result []string) {
	for _, series := range seriesArr {
		result = append(result, series+"_"+period)
	}
	return
}

// Return cartesian product of all arrays starting with prefix, joined by "join" ending with suffix
func joinedCartesian(mat [][]string, prefix, join, suffix string) (result []string) {
	// rows - number of arrays to join, rowsm1 (last index of array to join)
	rows := len(mat)
	rowsm1 := rows - 1

	// lens[i] - i-th row length - 1 (last i-th row column index)
	// curr[i] - current position in i-th row, we're processing N x M x ... positions
	// All possible combinations = Cartesian
	var (
		lens []int
		curr []int
	)
	for _, row := range mat {
		lens = append(lens, len(row)-1)
		curr = append(curr, 0)
	}

	// While not for all i curr[i] == lens[i]
	for {
		// Create one of output combinations
		str := prefix
		for i := 0; i < rows; i++ {
			str += mat[i][curr[i]]
			if i < rowsm1 {
				str += join
			}
		}
		str += suffix
		result = append(result, str)

		// Stop if for all i curr[i] == lens[i]
		// Which means we processed all possible combinations
		stop := true
		for i := 0; i < rows; i++ {
			if curr[i] < lens[i] {
				stop = false
				break
			}
		}
		if stop {
			break
		}

		// increase curr[i] for some i
		for i := 0; i < rows; i++ {
			// We can move to next permutation at this i
			if curr[i] < lens[i] {
				curr[i]++
				break
			} else {
				// We have to go to another row and zero all lower positions
				for j := 0; j <= i; j++ {
					curr[j] = 0
				}
			}
		}
	}

	// Retunrs "result" containing all possible permutations
	return
}

// Parse formula in format "=prefix;suffix;join;list1item1,list1item2,...;list2item1,list2item2,...;..."
func createSeriesFromFormula(def string) (result []string) {
	ary := strings.Split(def[1:], ";")
	if len(ary) < 4 {
		lib.Fatalf(
			"series formula must have at least 4 paramaters: "+
				"prefix, suffix, join, list, %v",
			def,
		)
	}

	// prefix, join value (how to connect strings from different arrays), suffix
	prefix, suffix, join := ary[0], ary[1], ary[2]

	// Create "matrix" of strings (not a real matrix because rows can have different counts)
	var matrix [][]string
	for _, list := range ary[3:] {
		vals := strings.Split(list, ",")
		matrix = append(matrix, vals)
	}

	// Create cartesian result with all possible combinations
	result = joinedCartesian(matrix, prefix, join, suffix)
	return
}

func sync(ctx *lib.Ctx, args []string) {
	// Strip function to be used by MapString
	stripFunc := func(x string) string { return strings.TrimSpace(x) }

	// Orgs & Repos
	sOrg := ""
	if len(args) > 0 {
		sOrg = args[0]
	}
	sRepo := ""
	if len(args) > 1 {
		sRepo = args[1]
	}
	org := lib.StringsMapToArray(stripFunc, strings.Split(sOrg, ","))
	repo := lib.StringsMapToArray(stripFunc, strings.Split(sRepo, ","))
	lib.Printf("gha2db_sync.go: Running on: %s/%s\n", strings.Join(org, "+"), strings.Join(repo, "+"))

	// Local or cron mode?
	dataPrefix := ctx.DataDir
	if ctx.Local {
		dataPrefix = "./"
	}
	cmdPrefix := ""
	if ctx.LocalCmd {
		cmdPrefix = "./"
	}

	// Connect to Postgres DB
	con := lib.PgConn(ctx)
	defer func() { lib.FatalOnError(con.Close()) }()

	// Get max event date from Postgres database
	var maxDtPtr *time.Time
	maxDtPg := ctx.DefaultStartDate
	if !ctx.ForceStartDate {
		lib.FatalOnError(lib.QueryRowSQL(con, ctx, "select max(dt) from gha_parsed").Scan(&maxDtPtr))
		if maxDtPtr != nil {
			maxDtPg = maxDtPtr.Add(1 * time.Hour)
		}
	}

	// Get max series date from TS database
	maxDtTSDB := ctx.DefaultStartDate
	if !ctx.ForceStartDate {
		table := "s" + ctx.LastSeries
		if lib.TableExists(con, ctx, table) {
			lib.FatalOnError(lib.QueryRowSQL(con, ctx, "select max(time) from "+table).Scan(&maxDtPtr))
			if maxDtPtr != nil {
				maxDtTSDB = *maxDtPtr
			}
		}
	}
	lib.Printf("Using start dates: pg: %s, tsdb: %s\n", lib.ToYMDHDate(maxDtPg), lib.ToYMDHDate(maxDtTSDB))

	// Create date range
	// Just to get into next GHA hour
	from := maxDtPg
	to := time.Now()
	nowHour := time.Now().Hour()
	fromDate := lib.ToYMDDate(from)
	fromHour := strconv.Itoa(from.Hour())
	toDate := lib.ToYMDDate(to)
	toHour := strconv.Itoa(to.Hour())

	// Get new GHAs
	if !ctx.SkipPDB {
		// Clear old DB logs
		lib.ClearDBLogs()

		// gha2db
		lib.Printf("GHA range: %s %s - %s %s\n", fromDate, fromHour, toDate, toHour)
		_, err := lib.ExecCommand(
			ctx,
			[]string{
				cmdPrefix + "gha2db",
				fromDate,
				fromHour,
				toDate,
				toHour,
				strings.Join(org, ","),
				strings.Join(repo, ","),
			},
			nil,
		)
		lib.FatalOnError(err)

		// Only run commits analysis for current DB here
		// We have updated repos to the newest state as 1st step in "devstats" call
		// We have also fetched all data from current GHA hour using "gha2db"
		// Now let's update new commits files (from newest hour)
		if !ctx.SkipGetRepos {
			lib.Printf("Update git commits\n")
			_, err = lib.ExecCommand(
				ctx,
				[]string{
					cmdPrefix + "get_repos",
				},
				map[string]string{
					"GHA2DB_PROCESS_COMMITS":  "1",
					"GHA2DB_PROJECTS_COMMITS": ctx.Project,
				},
			)
			lib.FatalOnError(err)
		}

		// GitHub API calls to get open issues state
		// It updates milestone and/or label(s) when different sice last comment state
		if !ctx.SkipGHAPI {
			lib.Printf("Update data from GitHub API\n")
			// Recompute views and DB summaries
			ctx.ExecFatal = false
			_, err = lib.ExecCommand(
				ctx,
				[]string{
					cmdPrefix + "ghapi2db",
				},
				nil,
			)
			ctx.ExecFatal = true
			if err != nil {
				lib.Printf("Error executing ghapi2db: %+v\n", err)
				fmt.Fprintf(os.Stderr, "Error executing ghapi2db: %+v\n", err)
			}
		}

		// Eventual postprocess SQL's from 'structure' call
		lib.Printf("Update structure\n")
		// Recompute views and DB summaries
		_, err = lib.ExecCommand(
			ctx,
			[]string{
				cmdPrefix + "structure",
			},
			map[string]string{
				"GHA2DB_SKIPTABLE": "1",
				"GHA2DB_MGETC":     "y",
			},
		)
		lib.FatalOnError(err)
	}

	// If ElasticSearch output is enabled
	if ctx.UseESRaw {
		// Regenerate points from this date
		esFromDate := fromDate
		esFromHour := fromHour
		if ctx.ResetESRaw {
			esFromDate = lib.ToYMDDate(ctx.DefaultStartDate)
			esFromHour = strconv.Itoa(ctx.DefaultStartDate.Hour())
		}
		lib.Printf("Update ElasticSearch raw index\n")
		lib.Printf("ES range: %s %s - %s %s\n", esFromDate, esFromHour, toDate, toHour)
		// Recompute views and DB summaries
		_, err := lib.ExecCommand(
			ctx,
			[]string{
				cmdPrefix + "gha2es",
				esFromDate,
				esFromHour,
				toDate,
				toHour,
			},
			nil,
		)
		lib.FatalOnError(err)
	}

	// Calc metric
	if !ctx.SkipTSDB || ctx.UseESOnly {
		metricsDir := dataPrefix + "metrics"
		if ctx.Project != "" {
			metricsDir += "/" + ctx.Project
		}
		// Regenerate points from this date
		if ctx.ResetTSDB {
			from = ctx.DefaultStartDate
		} else {
			from = maxDtTSDB
		}
		lib.Printf("TS range: %s - %s\n", lib.ToYMDHDate(from), lib.ToYMDHDate(to))

		// TSDB tags (repo groups template variable currently)
		if !ctx.SkipTags {
			if ctx.ResetTSDB || nowHour == 0 {
				_, err := lib.ExecCommand(ctx, []string{cmdPrefix + "tags"}, nil)
				lib.FatalOnError(err)
			} else {
				lib.Printf("Skipping `tags` recalculation, it is only computed once per day\n")
			}
		}
		// When resetting all TSDB data, adding new TS points will race for update TSDB structure
		// While we can just run "columns" once to ensure thay match tags output
		// Event if there are new columns after that - they will be very few not all of them to add at once
		if ctx.ResetTSDB && !ctx.SkipColumns {
			_, err := lib.ExecCommand(ctx, []string{cmdPrefix + "columns"}, nil)
			lib.FatalOnError(err)
		}

		// Annotations
		if !ctx.SkipAnnotations {
			if ctx.Project != "" && (ctx.ResetTSDB || nowHour == 0) {
				_, err := lib.ExecCommand(
					ctx,
					[]string{
						cmdPrefix + "annotations",
					},
					nil,
				)
				lib.FatalOnError(err)
			} else {
				lib.Printf("Skipping `annotations` recalculation, it is only computed once per day\n")
			}
		}

		// Get Quick Ranges from TSDB (it is filled by annotations command)
		quickRanges := lib.GetTagValues(con, ctx, "quick_ranges", "quick_ranges_suffix")
		lib.Printf("Quick ranges: %+v\n", quickRanges)

		// Read metrics configuration
		data, err := lib.ReadFile(ctx, dataPrefix+ctx.MetricsYaml)
		if err != nil {
			lib.FatalOnError(err)
			return
		}
		var allMetrics metrics
		lib.FatalOnError(yaml.Unmarshal(data, &allMetrics))

		// randomize metrics order
		if !ctx.SkipRand {
			allMetrics.randomize(ctx)
		}

		// Keep all histograms here
		var hists [][]string
		var envMaps []map[string]string
		onlyMetrics := false
		if len(ctx.OnlyMetrics) > 0 {
			onlyMetrics = true
		}
		skipMetrics := false
		if len(ctx.SkipMetrics) > 0 {
			skipMetrics = true
		}

		metricsList := []metric{}
		// Iterate all metrics
		for _, metric := range allMetrics.Metrics {
			if lib.ExcludedForProject(ctx.Project, metric.Project) {
				lib.Printf("Metric %s have project setting %s which is skipped for the current %s project\n", metric.Name, metric.Project, ctx.Project)
				continue
			}
			if metric.Histogram && metric.Drop != "" {
				lib.Fatalf("you cannot use drop series property on histogram metrics: %+v", metric)
			}
			if metric.MetricSQLs != nil {
				if metric.MetricSQL != "" {
					lib.Fatalf("you cannot use both 'sql' and 'sqls' fields'")
				}
				dropAdded := false
				for _, sql := range *metric.MetricSQLs {
					newMetric := metric
					newMetric.MetricSQLs = nil
					newMetric.MetricSQL = sql
					if !dropAdded {
						dropAdded = true
					} else {
						newMetric.Drop = ""
					}
					metricsList = append(metricsList, newMetric)
				}
				continue
			}
			metricsList = append(metricsList, metric)
		}

		// Iterate all metrics
		for _, metric := range metricsList {
			if metric.Disabled {
				continue
			}
			if onlyMetrics {
				_, ok := ctx.OnlyMetrics[metric.MetricSQL]
				if !ok {
					continue
				}
			}
			if skipMetrics {
				_, skip := ctx.SkipMetrics[metric.MetricSQL]
				if skip {
					continue
				}
			}
			dropProcessed := false
			// handle start_from (datetime) or last_hours (from now - N hours)
			fromDate := from
			if metric.StartFrom != nil && metric.LastHours > 0 {
				lib.Fatalf("you cannot use both StartFrom %v and LastHours %d", *metric.StartFrom, metric.LastHours)
			}
			if metric.StartFrom != nil && fromDate.Before(*metric.StartFrom) {
				fromDate = *metric.StartFrom
			}
			if metric.LastHours > 0 {
				dt := time.Now().Add(time.Hour * time.Duration(-metric.LastHours))
				if fromDate.Before(dt) {
					fromDate = dt
				}
			}
			if ctx.Debug > 0 && fromDate != from {
				lib.Printf("Using non-standard start date: %v, instead of %v\n", fromDate, from)
			}
			if fromDate != from && fromDate.After(to) {
				if ctx.Debug >= 0 {
					lib.Printf("Non-standard start date: %v (used instead of %v) is after end date %v, skipping\n", fromDate, from, to)
				}
				continue
			}
			extraParams := []string{}
			if ctx.ProjectScale != 1.0 {
				extraParams = append(extraParams, fmt.Sprintf("project_scale:%f", ctx.ProjectScale))
			}
			if metric.Histogram {
				extraParams = append(extraParams, "hist")
			}
			if metric.MultiValue {
				extraParams = append(extraParams, "multivalue")
			}
			if metric.EscapeValueName {
				extraParams = append(extraParams, "escape_value_name")
			}
			if metric.Desc != "" {
				extraParams = append(extraParams, "desc:"+metric.Desc)
			}
			if metric.MergeSeries != "" {
				extraParams = append(extraParams, "merge_series:"+metric.MergeSeries)
			}
			if metric.CustomData {
				extraParams = append(extraParams, "custom_data")
			}
			if metric.SeriesNameMap != nil {
				extraParams = append(extraParams, "series_name_map:"+fmt.Sprintf("%v", metric.SeriesNameMap))
			}
			periods := strings.Split(metric.Periods, ",")
			aggregate := metric.Aggregate
			if aggregate == "" {
				aggregate = "1"
			}
			if metric.AnnotationsRanges {
				extraParams = append(extraParams, "annotations_ranges")
				periods = quickRanges
				aggregate = "1"
			}
			aggregateArr := strings.Split(aggregate, ",")
			skips := strings.Split(metric.Skip, ",")
			skipMap := make(map[string]struct{})
			for _, skip := range skips {
				skipMap[skip] = struct{}{}
			}
			if !ctx.ResetTSDB && !ctx.ResetRanges {
				extraParams = append(extraParams, "skip_past")
			}
			for _, aggrStr := range aggregateArr {
				_, err := strconv.Atoi(aggrStr)
				lib.FatalOnError(err)
				aggrSuffix := aggrStr
				if aggrSuffix == "1" {
					aggrSuffix = ""
				}
				for _, period := range periods {
					periodAggr := period + aggrSuffix
					_, found := skipMap[periodAggr]
					if found {
						if ctx.Debug > 0 {
							lib.Printf("Skipped period %s\n", periodAggr)
						}
						continue
					}
					recalc := lib.ComputePeriodAtThisDate(ctx, period, to, metric.Histogram)
					if ctx.Debug > 0 {
						lib.Printf("Recalculate period \"%s%s\", hist %v for date to %v: %v\n", period, aggrSuffix, metric.Histogram, to, recalc)
					}
					if (!ctx.ResetTSDB || ctx.ComputePeriods != nil) && !recalc {
						lib.Printf("Skipping recalculating period \"%s%s\", hist %v for date to %v\n", period, aggrSuffix, metric.Histogram, to)
						continue
					}
					seriesNameOrFunc := metric.SeriesNameOrFunc
					if metric.AddPeriodToName {
						seriesNameOrFunc += "_" + periodAggr
					}
					// Histogram metrics usualy take long time, but executes single query, so there is no way to
					// Implement multi threading inside "calc_metric" call for them
					// So we're creating array of such metrics to be executed at the end - each in a separate go routine
					eParams := extraParams
					if ctx.EnableMetricsDrop && !dropProcessed {
						if metric.Drop != "" {
							eParams = append(eParams, "drop:"+metric.Drop)
						}
						dropProcessed = true
					}
					if metric.Histogram {
						lib.Printf("Scheduled histogram metric %v, period %v, desc: '%v', aggregate: '%v' ...\n", metric.Name, period, metric.Desc, aggrSuffix)
						hists = append(
							hists,
							[]string{
								cmdPrefix + "calc_metric",
								seriesNameOrFunc,
								fmt.Sprintf("%s/%s.sql", metricsDir, metric.MetricSQL),
								lib.ToYMDHDate(fromDate),
								lib.ToYMDHDate(to),
								periodAggr,
								strings.Join(extraParams, ","),
							},
						)
						envMaps = append(envMaps, metric.EnvMap)
					} else {
						lib.Printf("Calculate metric %v, period %v, desc: '%v', aggregate: '%v' ...\n", metric.Name, period, metric.Desc, aggrSuffix)
						_, err = lib.ExecCommand(
							ctx,
							[]string{
								cmdPrefix + "calc_metric",
								seriesNameOrFunc,
								fmt.Sprintf("%s/%s.sql", metricsDir, metric.MetricSQL),
								lib.ToYMDHDate(fromDate),
								lib.ToYMDHDate(to),
								periodAggr,
								strings.Join(eParams, ","),
							},
							metric.EnvMap,
						)
						lib.FatalOnError(err)
					}
				}
			}
		}
		// randomize histograms
		if !ctx.SkipRand {
			lib.Printf("Randomizing histogram metrics calculation order\n")
			rand.Seed(time.Now().UnixNano())
			rand.Shuffle(
				len(hists),
				func(i, j int) {
					hists[i], hists[j] = hists[j], hists[i]
					envMaps[i], envMaps[j] = envMaps[j], envMaps[i]
				},
			)
		}
		// Process histograms (possibly MT)
		// Get number of CPUs available
		thrN := lib.GetThreadsNum(ctx)
		if thrN > 1 {
			lib.Printf("Now processing %d histograms using MT%d version\n", len(hists), thrN)
			ch := make(chan bool)
			nThreads := 0
			for idx, hist := range hists {
				go calcHistogram(ch, ctx, hist, envMaps[idx])
				nThreads++
				if nThreads == thrN {
					<-ch
					nThreads--
				}
			}
			lib.Printf("Final threads join\n")
			for nThreads > 0 {
				<-ch
				nThreads--
			}
		} else {
			lib.Printf("Now processing %d histograms using ST version\n", len(hists))
			for idx, hist := range hists {
				calcHistogram(nil, ctx, hist, envMaps[idx])
			}
		}

		// TSDB ensure that calculated metric have all columns from tags
		if !ctx.SkipColumns {
			if ctx.RunColumns || ctx.ResetTSDB || nowHour == 0 {
				_, err := lib.ExecCommand(ctx, []string{cmdPrefix + "columns"}, nil)
				lib.FatalOnError(err)
			} else {
				lib.Printf("Skipping `columns` recalculation, it is only computed once per day\n")
			}
		}
	}

	// Vars (some tables/dashboards require vars calculation)
	if (!ctx.SkipPDB || ctx.UseESOnly) && !ctx.SkipVars {
		varsFN := os.Getenv("GHA2DB_VARS_FN_YAML")
		if varsFN == "" {
			varsFN = "sync_vars.yaml"
		}
		_, err := lib.ExecCommand(
			ctx,
			[]string{cmdPrefix + "vars"},
			map[string]string{
				"GHA2DB_VARS_FN_YAML": varsFN,
			},
		)
		lib.FatalOnError(err)
	}
	lib.Printf("Sync success\n")
}

// calcHistogram - calculate single histogram by calling "calc_metric" program with parameters from "hist"
func calcHistogram(ch chan bool, ctx *lib.Ctx, hist []string, envMap map[string]string) {
	if len(hist) != 7 {
		lib.Fatalf("calcHistogram, expected 7 strings, got: %d: %v", len(hist), hist)
	}
	lib.Printf(
		"Calculate histogram %s,%s,%s,%s,%s,%s ...\n",
		hist[1],
		hist[2],
		hist[3],
		hist[4],
		hist[5],
		hist[6],
	)
	// Execute "calc_metric"
	_, err := lib.ExecCommand(
		ctx,
		[]string{
			hist[0],
			hist[1],
			hist[2],
			hist[3],
			hist[4],
			hist[5],
			hist[6],
		},
		envMap,
	)
	lib.FatalOnError(err)
	// Synchronize go routine
	if ch != nil {
		ch <- true
	}
}

// Return per project args (if no args given) or get args from command line (if given)
// When no args given and no project set (via GHA2DB_PROJECT) it panics
func getSyncArgs(ctx *lib.Ctx, osArgs []string) []string {
	// User commandline override
	if len(osArgs) > 1 {
		return osArgs[1:]
	}

	// No user commandline, get args specific to project GHA2DB_PROJECT
	if ctx.Project == "" {
		lib.Fatalf(
			"you have to set project via GHA2DB_PROJECT environment variable if you provide no commandline arguments",
		)
	}
	// Local or cron mode?
	dataPrefix := ctx.DataDir
	if ctx.Local {
		dataPrefix = "./"
	}

	// Are we running from "devstats" which already sets ENV from projects.yaml?
	envSet := os.Getenv("ENV_SET") != ""

	// Read defined projects
	data, err := lib.ReadFile(ctx, dataPrefix+ctx.ProjectsYaml)
	if err != nil {
		lib.FatalOnError(err)
		return []string{}
	}
	var projects lib.AllProjects
	lib.FatalOnError(yaml.Unmarshal(data, &projects))
	proj, ok := projects.Projects[ctx.Project]
	if ok {
		if proj.StartDate != nil && !ctx.ForceStartDate {
			ctx.DefaultStartDate = *proj.StartDate
		}
		if !envSet && proj.Env != nil {
			for envK, envV := range proj.Env {
				lib.FatalOnError(os.Setenv(envK, envV))
			}
		}
		if proj.ProjectScale != nil && *proj.ProjectScale >= 0.0 {
			ctx.ProjectScale = *proj.ProjectScale
		}
		return proj.CommandLine
	}
	// No user commandline and project not found
	lib.Fatalf(
		"project '%s' is not defined in '%s'",
		ctx.Project,
		ctx.ProjectsYaml,
	)
	return []string{}
}

func main() {
	dtStart := time.Now()
	// Environment context parse
	var ctx lib.Ctx
	ctx.Init()
	sync(&ctx, getSyncArgs(&ctx, os.Args))
	dtEnd := time.Now()
	lib.Printf("Time: %v\n", dtEnd.Sub(dtStart))
}
