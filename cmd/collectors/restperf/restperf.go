package restperf

import (
	"fmt"
	rest2 "github.com/netapp/harvest/v2/cmd/collectors/rest"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/disk"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/fabricpool"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/fcp"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/fcvi"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/headroom"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/nic"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/volume"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/volumetag"
	"github.com/netapp/harvest/v2/cmd/collectors/restperf/plugins/vscan"
	"github.com/netapp/harvest/v2/cmd/poller/collector"
	"github.com/netapp/harvest/v2/cmd/poller/plugin"
	"github.com/netapp/harvest/v2/cmd/tools/rest"
	"github.com/netapp/harvest/v2/pkg/dict"
	"github.com/netapp/harvest/v2/pkg/errs"
	"github.com/netapp/harvest/v2/pkg/matrix"
	"github.com/netapp/harvest/v2/pkg/set"
	"github.com/netapp/harvest/v2/pkg/tree/node"
	"github.com/netapp/harvest/v2/pkg/util"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"golang.org/x/exp/maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	latencyIoReqd          = 10
	BILLION                = 1_000_000_000
	arrayKeyToken          = "#"
	objWorkloadClass       = "user_defined|system_defined"
	objWorkloadVolumeClass = "autovolume"
	timestampMetricName    = "timestamp"
)

var (
	constituentRegex = regexp.MustCompile(`^(.*)__(\d{4})$`)
)

var qosQuery = "api/cluster/counter/tables/qos"
var qosVolumeQuery = "api/cluster/counter/tables/qos_volume"
var qosDetailQuery = "api/cluster/counter/tables/qos_detail"
var qosDetailVolumeQuery = "api/cluster/counter/tables/qos_detail_volume"
var qosWorkloadQuery = "api/storage/qos/workloads"

var workloadDetailMetrics = []string{"resource_latency"}

var qosQueries = map[string]string{
	qosQuery:       qosQuery,
	qosVolumeQuery: qosVolumeQuery,
}
var qosDetailQueries = map[string]string{
	qosDetailQuery:       qosDetailQuery,
	qosDetailVolumeQuery: qosDetailVolumeQuery,
}

type RestPerf struct {
	*rest2.Rest     // provides: AbstractCollector, Client, Object, Query, TemplateFn, TemplateType
	perfProp        *perfProp
	archivedMetrics map[string]*rest2.Metric // Keeps metric definitions that are not found in the counter schema. These metrics may be available in future ONTAP versions.
}

type counter struct {
	name        string
	description string
	counterType string
	unit        string
	denominator string
}

type perfProp struct {
	isCacheEmpty        bool
	counterInfo         map[string]*counter
	latencyIoReqd       int
	qosLabels           map[string]string
	disableConstituents bool
}

type metricResponse struct {
	label   string
	value   string
	isArray bool
}

func init() {
	plugin.RegisterModule(&RestPerf{})
}

func (r *RestPerf) HarvestModule() plugin.ModuleInfo {
	return plugin.ModuleInfo{
		ID:  "harvest.collector.restperf",
		New: func() plugin.Module { return new(RestPerf) },
	}
}

func (r *RestPerf) Init(a *collector.AbstractCollector) error {

	var err error

	r.Rest = &rest2.Rest{AbstractCollector: a}

	r.perfProp = &perfProp{}

	r.InitProp()

	r.perfProp.counterInfo = make(map[string]*counter)
	r.archivedMetrics = make(map[string]*rest2.Metric)

	if err := r.InitClient(); err != nil {
		return err
	}

	if r.Prop.TemplatePath, err = r.LoadTemplate(); err != nil {
		return err
	}

	r.InitVars(a.Params)

	if err := collector.Init(r); err != nil {
		return err
	}

	if err := r.InitCache(); err != nil {
		return err
	}

	if err := r.InitMatrix(); err != nil {
		return err
	}

	if err := r.InitQOS(); err != nil {
		return err
	}

	r.Logger.Debug().
		Int("numMetrics", len(r.Prop.Metrics)).
		Str("timeout", r.Client.Timeout.String()).
		Msg("initialized cache")
	return nil
}

func (r *RestPerf) InitQOS() error {
	if isWorkloadObject(r.Prop.Query) || isWorkloadDetailObject(r.Prop.Query) {
		qosLabels := r.Params.GetChildS("qos_labels")
		if qosLabels == nil {
			return errs.New(errs.ErrMissingParam, "qos_labels")
		}
		r.perfProp.qosLabels = make(map[string]string)
		for _, label := range qosLabels.GetAllChildContentS() {

			display := strings.ReplaceAll(label, "-", "_")
			before, after, found := strings.Cut(label, "=>")
			if found {
				label = strings.TrimSpace(before)
				display = strings.TrimSpace(after)
			}
			r.perfProp.qosLabels[label] = display
		}
	}
	if counters := r.Params.GetChildS("counters"); counters != nil {
		refine := counters.GetChildS("refine")
		if refine != nil {
			withConstituents := refine.GetChildContentS("with_constituents")
			if withConstituents == "false" {
				r.perfProp.disableConstituents = true
			}
			withServiceLatency := refine.GetChildContentS("with_service_latency")
			if withServiceLatency != "false" {
				workloadDetailMetrics = append(workloadDetailMetrics, "service_time_latency")
			}
		}
	}
	return nil
}

func (r *RestPerf) InitMatrix() error {
	mat := r.Matrix[r.Object]
	// init perf properties
	r.perfProp.latencyIoReqd = r.loadParamInt("latency_io_reqd", latencyIoReqd)
	r.perfProp.isCacheEmpty = true
	// overwrite from abstract collector
	mat.Object = r.Prop.Object
	// Add system (cluster) name
	mat.SetGlobalLabel("cluster", r.Client.Cluster().Name)
	if r.Params.HasChildS("labels") {
		for _, l := range r.Params.GetChildS("labels").GetChildren() {
			mat.SetGlobalLabel(l.GetNameS(), l.GetContentS())
		}
	}

	// Add metadata metric for skips/numPartials
	_, _ = r.Metadata.NewMetricUint64("skips")
	_, _ = r.Metadata.NewMetricUint64("numPartials")
	return nil
}

// load workload_class or use defaultValue
func (r *RestPerf) loadWorkloadClassQuery(defaultValue string) string {

	var x *node.Node

	name := "workload_class"

	if x = r.Params.GetChildS(name); x != nil {
		v := x.GetAllChildContentS()
		if len(v) == 0 {
			r.Logger.Debug().
				Str("name", name).
				Str("defaultValue", defaultValue).
				Send()
			return defaultValue
		}
		slices.Sort(v)
		s := strings.Join(v, "|")
		r.Logger.Debug().
			Str("name", name).
			Str("value", s).
			Send()
		return s
	}
	r.Logger.Debug().
		Str("name", name).
		Str("defaultValue", defaultValue).
		Send()
	return defaultValue
}

// load an int parameter or use defaultValue
func (r *RestPerf) loadParamInt(name string, defaultValue int) int {

	var (
		x string
		n int
		e error
	)

	if x = r.Params.GetChildContentS(name); x != "" {
		if n, e = strconv.Atoi(x); e == nil {
			r.Logger.Debug().Msgf("using %s = [%d]", name, n)
			return n
		}
		r.Logger.Warn().Msgf("invalid parameter %s = [%s] (expected integer)", name, x)
	}

	r.Logger.Debug().Str("name", name).Str("defaultValue", strconv.Itoa(defaultValue)).Msg("using values")
	return defaultValue
}

func (r *RestPerf) PollCounter() (map[string]*matrix.Matrix, error) {
	var (
		err     error
		records []gjson.Result
	)

	href := rest.NewHrefBuilder().
		APIPath(r.Prop.Query).
		ReturnTimeout(r.Prop.ReturnTimeOut).
		Build()
	r.Logger.Debug().Str("href", href).Send()
	if href == "" {
		return nil, errs.New(errs.ErrConfig, "empty url")
	}

	apiT := time.Now()
	r.Client.Metadata.Reset()

	records, err = rest.Fetch(r.Client, href)
	if err != nil {
		return r.handleError(err, href)
	}

	return r.pollCounter(records, time.Since(apiT))
}

func (r *RestPerf) pollCounter(records []gjson.Result, apiD time.Duration) (map[string]*matrix.Matrix, error) {
	var (
		err           error
		counterSchema gjson.Result
		parseT        time.Time
	)

	mat := r.Matrix[r.Object]
	firstRecord := records[0]

	parseT = time.Now()

	if firstRecord.Exists() {
		counterSchema = firstRecord.Get("counter_schemas")
	} else {
		return nil, errs.New(errs.ErrConfig, "no data found")
	}
	seenMetrics := make(map[string]bool)

	// populate denominator metric to prop metrics
	counterSchema.ForEach(func(_, c gjson.Result) bool {
		if !c.IsObject() {
			r.Logger.Warn().Str("type", c.Type.String()).Msg("Counter is not object, skipping")
			return true
		}

		name := strings.Clone(c.Get("name").String())
		dataType := strings.Clone(c.Get("type").String())

		if p := r.GetOverride(name); p != "" {
			dataType = p
		}

		// Check if the metric was previously archived and restore it
		if archivedMetric, found := r.archivedMetrics[name]; found {
			r.Prop.Metrics[name] = archivedMetric
			delete(r.archivedMetrics, name) // Remove from archive after restoring
			r.Logger.Info().
				Str("key", name).
				Msg("Metric found in archive. Restore it")
		}

		if _, has := r.Prop.Metrics[name]; has {
			if strings.Contains(dataType, "string") {
				if _, ok := r.Prop.InstanceLabels[name]; !ok {
					r.Prop.InstanceLabels[name] = r.Prop.Counters[name]
				}
				// set exportable as false
				r.Prop.Metrics[name].Exportable = false
				return true
			}
			d := strings.Clone(c.Get("denominator.name").String())
			if d != "" {
				if _, has := r.Prop.Metrics[d]; !has {
					if isWorkloadDetailObject(r.Prop.Query) {
						// It is not needed because 'ops' is used as the denominator in latency calculations.
						if d == "visits" {
							return true
						}
					}
					// export false
					m := &rest2.Metric{Label: "", Name: d, MetricType: "", Exportable: false}
					r.Prop.Metrics[d] = m
				}
			}
		}
		return true
	})

	counterSchema.ForEach(func(_, c gjson.Result) bool {

		if !c.IsObject() {
			r.Logger.Warn().Str("type", c.Type.String()).Msg("Counter is not object, skipping")
			return true
		}

		name := strings.Clone(c.Get("name").String())
		if _, has := r.Prop.Metrics[name]; has {
			seenMetrics[name] = true
			if _, ok := r.perfProp.counterInfo[name]; !ok {
				r.perfProp.counterInfo[name] = &counter{
					name:        name,
					description: strings.Clone(c.Get("description").String()),
					counterType: strings.Clone(c.Get("type").String()),
					unit:        strings.Clone(c.Get("unit").String()),
					denominator: strings.Clone(c.Get("denominator.name").String()),
				}
				if p := r.GetOverride(name); p != "" {
					r.perfProp.counterInfo[name].counterType = p
				}
			}
		}

		return true
	})

	for name, metric := range r.Prop.Metrics {
		if !seenMetrics[name] {
			r.archivedMetrics[name] = metric
			// Log the metric that is not present in counterSchema.
			r.Logger.Warn().
				Str("key", name).
				Msg("Metric not found in counterSchema")
			delete(r.Prop.Metrics, name)
		}
	}

	// Create an artificial metric to hold timestamp of each instance data.
	// The reason we don't keep a single timestamp for the whole data
	// is because we might get instances in different batches
	if mat.GetMetric(timestampMetricName) == nil {
		m, err := mat.NewMetricFloat64(timestampMetricName)
		if err != nil {
			r.Logger.Error().Err(err).Msg("add timestamp metric")
		}
		m.SetProperty("raw")
		m.SetExportable(false)
	}

	_, err = r.processWorkLoadCounter()
	if err != nil {
		return nil, err
	}

	// update metadata for collector logs
	_ = r.Metadata.LazySetValueInt64("api_time", "counter", apiD.Microseconds())
	_ = r.Metadata.LazySetValueInt64("parse_time", "counter", time.Since(parseT).Microseconds())
	_ = r.Metadata.LazySetValueUint64("metrics", "counter", uint64(len(r.perfProp.counterInfo)))
	_ = r.Metadata.LazySetValueUint64("bytesRx", "counter", r.Client.Metadata.BytesRx)
	_ = r.Metadata.LazySetValueUint64("numCalls", "counter", r.Client.Metadata.NumCalls)

	return nil, nil
}

func parseProps(instanceData gjson.Result) map[string]gjson.Result {
	var props = map[string]gjson.Result{
		"id": gjson.Get(instanceData.String(), "id"),
	}

	instanceData.ForEach(func(key, v gjson.Result) bool {
		keyS := key.String()
		if keyS == "properties" {
			v.ForEach(func(_, each gjson.Result) bool {
				key := each.Get("name").String()
				value := each.Get("value")
				props[key] = value
				return true
			})
			return false
		}
		return true
	})
	return props
}

func parseProperties(instanceData gjson.Result, property string) gjson.Result {
	var (
		result gjson.Result
	)

	if property == "id" {
		value := gjson.Get(instanceData.String(), "id")
		return value
	}

	instanceData.ForEach(func(key, v gjson.Result) bool {
		keyS := key.String()
		if keyS == "properties" {
			v.ForEach(func(_, each gjson.Result) bool {
				if each.Get("name").String() == property {
					value := each.Get("value")
					result = value
					return false
				}
				return true
			})
			return false
		}
		return true
	})

	return result
}

func parseMetricResponses(instanceData gjson.Result, metric map[string]*rest2.Metric) map[string]*metricResponse {
	var (
		mapMetricResponses = make(map[string]*metricResponse)
		numWant            = len(metric)
		numSeen            = 0
	)
	instanceData.ForEach(func(key, v gjson.Result) bool {
		keyS := key.String()
		if keyS == "counters" {
			v.ForEach(func(_, each gjson.Result) bool {
				if numSeen == numWant {
					return false
				}
				name := each.Get("name").String()
				_, ok := metric[name]
				if !ok {
					return true
				}
				value := each.Get("value").String()
				if value != "" {
					mapMetricResponses[name] = &metricResponse{value: strings.Clone(value), label: ""}
					numSeen++
					return true
				}
				values := each.Get("values").String()
				labels := each.Get("labels").String()
				if values != "" {
					mapMetricResponses[name] = &metricResponse{
						value:   util.ArrayMetricToString(strings.Clone(values)),
						label:   util.ArrayMetricToString(strings.Clone(labels)),
						isArray: true,
					}
					numSeen++
					return true
				}
				subCounters := each.Get("counters")
				if !subCounters.IsArray() {
					return true
				}

				// handle sub metrics
				subLabelsS := strings.Clone(labels)
				subLabelsS = util.ArrayMetricToString(subLabelsS)
				subLabelSlice := strings.Split(subLabelsS, ",")
				var finalLabels []string
				var finalValues []string
				subCounters.ForEach(func(_, subCounter gjson.Result) bool {
					label := strings.Clone(subCounter.Get("label").String())
					subValues := subCounter.Get("values").String()
					m := util.ArrayMetricToString(strings.Clone(subValues))
					ms := strings.Split(m, ",")
					if len(ms) > len(subLabelSlice) {
						return false
					}
					for i := range ms {
						finalLabels = append(finalLabels, subLabelSlice[i]+arrayKeyToken+label)
					}
					finalValues = append(finalValues, ms...)
					return true
				})
				if len(finalLabels) == len(finalValues) {
					mr := metricResponse{
						value:   strings.Join(finalValues, ","),
						label:   strings.Join(finalLabels, ","),
						isArray: true,
					}
					mapMetricResponses[name] = &mr
				}
				return true
			})
		}
		return true
	})
	return mapMetricResponses
}

func parseMetricResponse(instanceData gjson.Result, metric string) *metricResponse {
	instanceDataS := instanceData.String()
	t := gjson.Get(instanceDataS, "counters.#.name")
	for _, name := range t.Array() {
		if name.String() != metric {
			continue
		}
		metricPath := "counters.#(name=" + metric + ")"
		many := gjson.Parse(instanceDataS)
		value := many.Get(metricPath + ".value")
		values := many.Get(metricPath + ".values")
		labels := many.Get(metricPath + ".labels")
		subLabels := many.Get(metricPath + ".counters.#.label")
		subValues := many.Get(metricPath + ".counters.#.values")
		if value.String() != "" {
			return &metricResponse{value: strings.Clone(value.String()), label: ""}
		}
		if values.String() != "" {
			return &metricResponse{
				value:   util.ArrayMetricToString(strings.Clone(values.String())),
				label:   util.ArrayMetricToString(strings.Clone(labels.String())),
				isArray: true,
			}
		}

		// check for sub metrics
		if subLabels.String() != "" {
			var finalLabels []string
			var finalValues []string
			subLabelsS := strings.Clone(labels.String())
			subLabelsS = util.ArrayMetricToString(subLabelsS)
			subLabelSlice := strings.Split(subLabelsS, ",")
			ls := subLabels.Array()
			vs := subValues.Array()
			var vLen int
			for i, v := range vs {
				label := strings.Clone(ls[i].String())
				m := util.ArrayMetricToString(strings.Clone(v.String()))
				ms := strings.Split(m, ",")
				for range ms {
					finalLabels = append(finalLabels, label+arrayKeyToken+subLabelSlice[vLen])
					vLen++
				}
				if vLen > len(subLabelSlice) {
					break
				}
				finalValues = append(finalValues, ms...)
			}
			if vLen == len(subLabelSlice) {
				return &metricResponse{value: strings.Join(finalValues, ","), label: strings.Join(finalLabels, ","), isArray: true}
			}
		}
	}
	return &metricResponse{}
}

// GetOverride override counter property
func (r *RestPerf) GetOverride(counter string) string {
	if o := r.Params.GetChildS("override"); o != nil {
		return o.GetChildContentS(counter)
	}
	return ""
}

func (r *RestPerf) processWorkLoadCounter() (map[string]*matrix.Matrix, error) {
	var err error
	mat := r.Matrix[r.Object]
	// for these two objects, we need to create latency/ops counters for each of the workload layers
	// their original counters will be discarded
	if isWorkloadDetailObject(r.Prop.Query) {

		for name, metric := range r.Prop.Metrics {
			metr, ok := mat.GetMetrics()[name]
			if !ok {
				if metr, err = mat.NewMetricFloat64(name, metric.Label); err != nil {
					r.Logger.Error().Err(err).
						Str("name", name).
						Msg("NewMetricFloat64")
				}
			}
			metr.SetExportable(metric.Exportable)
		}

		var service, wait, ops *matrix.Metric

		if service = mat.GetMetric("service_time"); service == nil {
			r.Logger.Error().Msg("metric [service_time] required to calculate workload missing")
		}

		if wait = mat.GetMetric("wait_time"); wait == nil {
			r.Logger.Error().Msg("metric [wait-time] required to calculate workload missing")
		}

		if service == nil || wait == nil {
			return nil, errs.New(errs.ErrMissingParam, "workload metrics")
		}

		if ops = mat.GetMetric("ops"); ops == nil {
			if _, err = mat.NewMetricFloat64("ops"); err != nil {
				return nil, err
			}
			r.perfProp.counterInfo["ops"] = &counter{
				name:        "ops",
				description: "",
				counterType: "rate",
				unit:        "per_sec",
				denominator: "",
			}
		}

		service.SetExportable(false)
		wait.SetExportable(false)

		resourceMap := r.Params.GetChildS("resource_map")
		if resourceMap == nil {
			return nil, errs.New(errs.ErrMissingParam, "resource_map")
		}
		for _, x := range resourceMap.GetChildren() {
			for _, wm := range workloadDetailMetrics {
				name := x.GetNameS() + wm
				resource := x.GetContentS()

				if m := mat.GetMetric(name); m != nil {
					continue
				}
				m, err := mat.NewMetricFloat64(name, wm)
				if err != nil {
					return nil, err
				}
				r.perfProp.counterInfo[name] = &counter{
					name:        wm,
					description: "",
					counterType: r.perfProp.counterInfo[service.GetName()].counterType,
					unit:        r.perfProp.counterInfo[service.GetName()].unit,
					denominator: "ops",
				}
				m.SetLabel("resource", resource)
			}
		}
	}
	return r.Matrix, nil
}

func (r *RestPerf) PollData() (map[string]*matrix.Matrix, error) {
	var (
		err         error
		perfRecords []rest.PerfRecord
		startTime   time.Time
	)

	mat := r.Matrix[r.Object]
	if len(mat.GetInstances()) == 0 {
		return nil, errs.New(errs.ErrNoInstance, "no "+r.Object+" instances fetched in PollInstance")
	}

	timestamp := r.Matrix[r.Object].GetMetric(timestampMetricName)
	if timestamp == nil {
		return nil, errs.New(errs.ErrConfig, "missing timestamp metric")
	}

	startTime = time.Now()
	r.Client.Metadata.Reset()

	dataQuery := path.Join(r.Prop.Query, "rows")

	var filter []string
	// Sort filters so that the href is deterministic
	metrics := maps.Keys(r.Prop.Metrics)
	slices.Sort(metrics)

	filter = append(filter, "counters.name="+strings.Join(metrics, "|"))

	href := rest.NewHrefBuilder().
		APIPath(dataQuery).
		Fields([]string{"*"}).
		Filter(filter).
		ReturnTimeout(r.Prop.ReturnTimeOut).
		Build()

	r.Logger.Debug().Str("href", href).Send()
	if href == "" {
		return nil, errs.New(errs.ErrConfig, "empty url")
	}

	err = rest.FetchRestPerfData(r.Client, href, &perfRecords)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch href=%s %w", href, err)
	}

	return r.pollData(startTime, perfRecords)
}

// getMetric retrieves the metric associated with the given key from the current matrix (curMat).
// If the metric does not exist in curMat, it is created with the provided display settings.
// The function also ensures that the same metric exists in the previous matrix (prevMat) to
// allow for subsequent calculations (e.g., prevMetric - curMetric).
// This is particularly important in cases such as ONTAP upgrades, where curMat may contain
// additional metrics that are not present in prevMat. If prevMat does not have the metric,
// it is created to prevent a panic when attempting to perform calculations with non-existent metrics.
//
// This metric creation process within RestPerf is necessary during PollData because the information about whether a metric
// is an array is not available in the RestPerf PollCounter. The determination of whether a metric is an array
// is made by examining the actual data in RestPerf. Therefore, metric creation in RestPerf is performed during
// the poll data phase, and special handling is required for such cases.
//
// The function returns the current metric and any error encountered during its retrieval or creation.
func (r *RestPerf) getMetric(curMat *matrix.Matrix, prevMat *matrix.Matrix, key string, display ...string) (*matrix.Metric, error) {
	var err error
	curMetric := curMat.GetMetric(key)
	if curMetric == nil {
		curMetric, err = curMat.NewMetricFloat64(key, display...)
		if err != nil {
			return nil, err
		}
	}

	prevMetric := prevMat.GetMetric(key)
	if prevMetric == nil {
		_, err = prevMat.NewMetricFloat64(key, display...)
		if err != nil {
			return nil, err
		}
	}
	return curMetric, nil
}

func (r *RestPerf) pollData(startTime time.Time, perfRecords []rest.PerfRecord) (map[string]*matrix.Matrix, error) {
	var (
		count        uint64
		apiD, parseD time.Duration
		err          error
		instanceKeys []string
		skips        int
		numPartials  uint64
		instIndex    int
		ts           float64
		prevMat      *matrix.Matrix
		curMat       *matrix.Matrix
	)

	prevMat = r.Matrix[r.Object]
	// clone matrix without numeric data
	curMat = prevMat.Clone(matrix.With{Data: false, Metrics: true, Instances: true, ExportInstances: true})
	curMat.Reset()
	instanceKeys = r.Prop.InstanceKeys

	apiD = time.Since(startTime)
	// init current time
	ts = float64(startTime.UnixNano()) / BILLION

	startTime = time.Now()
	instIndex = -1

	if len(perfRecords) == 0 {
		return nil, errs.New(errs.ErrNoInstance, "no "+r.Object+" instances on cluster")
	}

	for _, perfRecord := range perfRecords {
		pr := perfRecord.Records
		t := perfRecord.Timestamp

		if t != 0 {
			ts = float64(t) / BILLION
		} else {
			r.Logger.Warn().Msg("Missing timestamp in response")
		}

		pr.ForEach(func(_, instanceData gjson.Result) bool {
			var (
				instanceKey     string
				instance        *matrix.Instance
				isHistogram     bool
				histogramMetric *matrix.Metric
			)
			instIndex++

			if !instanceData.IsObject() {
				r.Logger.Warn().Str("type", instanceData.Type.String()).Msg("Instance data is not object, skipping")
				return true
			}

			props := parseProps(instanceData)

			if len(instanceKeys) != 0 {
				// extract instance key(s)
				for _, k := range instanceKeys {
					value, ok := props[k]
					if ok {
						instanceKey += strings.Clone(value.String())
					} else {
						r.Logger.Warn().Str("key", k).Msg("missing key")
					}
				}

				if instanceKey == "" {
					return true
				}
			}

			var layer = "" // latency layer (resource) for workloads

			// special case for these two objects
			// we need to process each latency layer for each instance/counter
			if isWorkloadDetailObject(r.Prop.Query) {

				// example instanceKey : umeng-aff300-02:test-wid12022.CPU_dblade
				i := strings.Index(instanceKey, ":")
				instanceKey = instanceKey[i+1:]
				before, after, found := strings.Cut(instanceKey, ".")
				if found {
					instanceKey = before
					layer = after
				} else {
					r.Logger.Warn().
						Str("instanceKey", instanceKey).
						Msg("instanceKey has unexpected format")
					return true
				}

				for _, wm := range workloadDetailMetrics {
					mLayer := layer + wm
					if l := curMat.GetMetric(mLayer); l == nil {
						return true
					}
				}
			}

			if r.Params.GetChildContentS("only_cluster_instance") != "true" {
				if instanceKey == "" {
					return true
				}
			}

			instance = curMat.GetInstance(instanceKey)
			if instance == nil {
				if !isWorkloadObject(r.Prop.Query) && !isWorkloadDetailObject(r.Prop.Query) {
					r.Logger.Warn().
						Str("instanceKey", instanceKey).
						Msg("Skip instanceKey, not found in cache")
				}
				return true
			}

			// check for partial aggregation
			if instanceData.Get("aggregation.complete").String() == "false" {
				instance.SetPartial(true)
				numPartials++
			}

			for label, display := range r.Prop.InstanceLabels {
				value, ok := props[label]
				if ok {
					if value.IsArray() {
						var labelArray []string
						value.ForEach(func(_, r gjson.Result) bool {
							labelString := strings.Clone(r.String())
							labelArray = append(labelArray, labelString)
							return true
						})
						instance.SetLabel(display, strings.Join(labelArray, ","))
					} else {
						instance.SetLabel(display, strings.Clone(value.String()))
					}
					count++
				} else {
					// check for label value in metric
					f := parseMetricResponse(instanceData, label)
					if f.value != "" {
						instance.SetLabel(display, f.value)
						count++
					} else {
						// ignore physical_disk_id logging as in some of 9.12 versions, this field may be absent
						if r.Prop.Query == "api/cluster/counter/tables/disk:constituent" && label == "physical_disk_id" {
							r.Logger.Debug().Str("instanceKey", instanceKey).Str("label", label).Msg("Missing label value")
						} else {
							r.Logger.Error().Str("instanceKey", instanceKey).Str("label", label).Msg("Missing label value")
						}
					}
				}
			}

			metricResponses := parseMetricResponses(instanceData, r.Prop.Metrics)

			for name, metric := range r.Prop.Metrics {
				f, ok := metricResponses[name]
				if ok {
					// special case for workload_detail
					if isWorkloadDetailObject(r.Prop.Query) {
						for _, wm := range workloadDetailMetrics {
							wMetric := curMat.GetMetric(layer + wm)
							switch {
							case wm == "resource_latency" && (name == "wait_time" || name == "service_time"):
								if err := wMetric.AddValueString(instance, f.value); err != nil {
									r.Logger.Error().
										Stack().
										Err(err).
										Str("name", name).
										Str("value", f.value).
										Msg("Add resource_latency failed")
								} else {
									count++
								}
								continue
							case wm == "service_time_latency" && name == "service_time":
								if err = wMetric.SetValueString(instance, f.value); err != nil {
									r.Logger.Error().
										Stack().
										Err(err).
										Str("name", name).
										Str("value", f.value).
										Msg("Add service_time_latency failed")
								} else {
									count++
								}
							case wm == "wait_time_latency" && name == "wait_time":
								if err = wMetric.SetValueString(instance, f.value); err != nil {
									r.Logger.Error().
										Stack().
										Err(err).
										Str("name", name).
										Str("value", f.value).
										Msg("Add wait_time_latency failed")
								} else {
									count++
								}
							}
						}
						continue
					}
					if f.isArray {
						labels := strings.Split(f.label, ",")
						values := strings.Split(f.value, ",")

						if len(labels) != len(values) {
							// warn & skip
							r.Logger.Warn().
								Str("labels", f.label).
								Str("value", f.value).
								Msg("labels don't match parsed values")
							continue
						}

						// ONTAP does not have a `type` for histogram. Harvest tests the `desc` field to determine
						// if a counter is a histogram
						isHistogram = false
						description := strings.ToLower(r.perfProp.counterInfo[name].description)
						if len(labels) > 0 && strings.Contains(description, "histogram") {
							key := name + ".bucket"
							histogramMetric, err = r.getMetric(curMat, prevMat, key, metric.Label)
							if err != nil {
								r.Logger.Error().Err(err).Str("key", key).Msg("unable to create histogram metric")
								continue
							}
							histogramMetric.SetArray(true)
							histogramMetric.SetExportable(metric.Exportable)
							histogramMetric.SetBuckets(&labels)
							isHistogram = true
						}

						for i, label := range labels {
							k := name + arrayKeyToken + label
							metr, ok := curMat.GetMetrics()[k]
							if !ok {
								if metr, err = r.getMetric(curMat, prevMat, k, metric.Label); err != nil {
									r.Logger.Error().Err(err).
										Str("name", k).
										Msg("NewMetricFloat64")
									continue
								}
								if x := strings.Split(label, arrayKeyToken); len(x) == 2 {
									metr.SetLabel("metric", x[0])
									metr.SetLabel("submetric", x[1])
								} else {
									metr.SetLabel("metric", label)
								}
								// differentiate between array and normal counter
								metr.SetArray(true)
								metr.SetExportable(metric.Exportable)
								if isHistogram {
									// Save the index of this label so the labels can be exported in order
									metr.SetLabel("comment", strconv.Itoa(i))
									// Save the bucket name so the flattened metrics can find their bucket when exported
									metr.SetLabel("bucket", name+".bucket")
									metr.SetHistogram(true)
								}
							}
							if err = metr.SetValueString(instance, values[i]); err != nil {
								r.Logger.Error().
									Err(err).
									Str("name", name).
									Str("label", label).
									Str("value", values[i]).
									Int("instIndex", instIndex).
									Msg("Set value failed")
								continue
							}
							count++
						}
					} else {
						metr, ok := curMat.GetMetrics()[name]
						if !ok {
							if metr, err = r.getMetric(curMat, prevMat, name, metric.Label); err != nil {
								r.Logger.Error().Err(err).
									Str("name", name).
									Int("instIndex", instIndex).
									Msg("NewMetricFloat64")
							}
						}
						metr.SetExportable(metric.Exportable)
						if c, err := strconv.ParseFloat(f.value, 64); err == nil {
							if err = metr.SetValueFloat64(instance, c); err != nil {
								r.Logger.Error().Err(err).
									Str("key", metric.Name).
									Str("metric", metric.Label).
									Int("instIndex", instIndex).
									Msg("Unable to set float key on metric")
							}
						} else {
							r.Logger.Error().Err(err).
								Str("key", metric.Name).
								Str("metric", metric.Label).
								Int("instIndex", instIndex).
								Msg("Unable to parse float value")
						}
						count++
					}
				} else {
					r.Logger.Warn().Str("counter", name).Msg("Counter is missing or unable to parse.")
				}
			}
			if err = curMat.GetMetric(timestampMetricName).SetValueFloat64(instance, ts); err != nil {
				r.Logger.Error().Err(err).Msg("Failed to set timestamp")
			}

			return true
		})
	}

	if isWorkloadDetailObject(r.Prop.Query) {
		if err := r.getParentOpsCounters(curMat); err != nil {
			// no point to continue as we can't calculate the other counters
			return nil, err
		}
	}

	parseD = time.Since(startTime)
	_ = r.Metadata.LazySetValueInt64("api_time", "data", apiD.Microseconds())
	_ = r.Metadata.LazySetValueInt64("parse_time", "data", parseD.Microseconds())
	_ = r.Metadata.LazySetValueUint64("metrics", "data", count)
	_ = r.Metadata.LazySetValueUint64("instances", "data", uint64(len(curMat.GetInstances())))
	_ = r.Metadata.LazySetValueUint64("bytesRx", "data", r.Client.Metadata.BytesRx)
	_ = r.Metadata.LazySetValueUint64("numCalls", "data", r.Client.Metadata.NumCalls)
	_ = r.Metadata.LazySetValueUint64("numPartials", "data", numPartials)

	r.AddCollectCount(count)

	// skip calculating from delta if no data from previous poll
	if r.perfProp.isCacheEmpty {
		r.Logger.Debug().Msg("skip postprocessing until next poll (previous cache empty)")
		r.Matrix[r.Object] = curMat
		r.perfProp.isCacheEmpty = false
		return nil, nil
	}

	calcStart := time.Now()

	// cache raw data for next poll
	cachedData := curMat.Clone(matrix.With{Data: true, Metrics: true, Instances: true, ExportInstances: true, PartialInstances: true})

	orderedNonDenominatorMetrics := make([]*matrix.Metric, 0, len(curMat.GetMetrics()))
	orderedNonDenominatorKeys := make([]string, 0, len(orderedNonDenominatorMetrics))

	orderedDenominatorMetrics := make([]*matrix.Metric, 0, len(curMat.GetMetrics()))
	orderedDenominatorKeys := make([]string, 0, len(orderedDenominatorMetrics))

	for key, metric := range curMat.GetMetrics() {
		if metric.GetName() != timestampMetricName && metric.Buckets() == nil {
			counter := r.counterLookup(metric, key)
			if counter != nil {
				if counter.denominator == "" {
					// does not require base counter
					orderedNonDenominatorMetrics = append(orderedNonDenominatorMetrics, metric)
					orderedNonDenominatorKeys = append(orderedNonDenominatorKeys, key)
				} else {
					// does require base counter
					orderedDenominatorMetrics = append(orderedDenominatorMetrics, metric)
					orderedDenominatorKeys = append(orderedDenominatorKeys, key)
				}
			} else {
				r.Logger.Warn().Str("counter", metric.GetName()).Msg("Counter is missing or unable to parse")
			}
		}
	}

	// order metrics, such that those requiring base counters are processed last
	orderedMetrics := orderedNonDenominatorMetrics
	orderedMetrics = append(orderedMetrics, orderedDenominatorMetrics...)
	orderedKeys := orderedNonDenominatorKeys
	orderedKeys = append(orderedKeys, orderedDenominatorKeys...)

	// Calculate timestamp delta first since many counters require it for postprocessing.
	// Timestamp has "raw" property, so it isn't post-processed automatically
	if _, err = curMat.Delta("timestamp", prevMat, r.Logger); err != nil {
		r.Logger.Error().Err(err).Msg("(timestamp) calculate delta:")
	}

	var base *matrix.Metric
	var totalSkips int

	for i, metric := range orderedMetrics {
		key := orderedKeys[i]
		counter := r.counterLookup(metric, key)
		if counter == nil {
			r.Logger.Error().Err(err).Str("counter", metric.GetName()).Msg("Missing counter:")
			continue
		}
		property := counter.counterType
		// used in aggregator plugin
		metric.SetProperty(property)
		// used in volume.go plugin
		metric.SetComment(counter.denominator)

		// raw/string - submit without post-processing
		if property == "raw" || property == "string" {
			continue
		}

		// all other properties - first calculate delta
		if skips, err = curMat.Delta(key, prevMat, r.Logger); err != nil {
			r.Logger.Error().Err(err).Str("key", key).Msg("Calculate delta")
			continue
		}
		totalSkips += skips

		// DELTA - subtract previous value from current
		if property == "delta" {
			// already done
			continue
		}

		// RATE - delta, normalized by elapsed time
		if property == "rate" {
			// defer calculation, so we can first calculate averages/percents
			// Note: calculating rate before averages are averages/percentages are calculated
			// used to be a bug in Harvest 2.0 (Alpha, RC1, RC2) resulting in very high latency values
			continue
		}

		// For the next two properties we need base counters
		// We assume that delta of base counters is already calculated
		if base = curMat.GetMetric(counter.denominator); base == nil {
			if isWorkloadDetailObject(r.Prop.Query) {
				// The workload detail generates metrics at the resource level. The 'service_time' and 'wait_time' metrics are used as raw values for these resource-level metrics. Their denominator, 'visits', is not collected; therefore, a check is added here to prevent warnings.
				// There is no need to cook these metrics further.
				if key == "service_time" || key == "wait_time" {
					continue
				}
			}
			r.Logger.Warn().
				Str("key", key).
				Str("property", property).
				Str("denominator", counter.denominator).
				Int("instIndex", instIndex).
				Msg("Base counter missing")
			continue
		}

		// remaining properties: average and percent
		//
		// AVERAGE - delta, divided by base-counter delta
		//
		// PERCENT - average * 100
		// special case for latency counter: apply minimum number of iops as threshold
		if property == "average" || property == "percent" {

			if strings.HasSuffix(metric.GetName(), "latency") {
				skips, err = curMat.DivideWithThreshold(key, counter.denominator, r.perfProp.latencyIoReqd, cachedData, prevMat, timestampMetricName, r.Logger)
			} else {
				skips, err = curMat.Divide(key, counter.denominator)
			}

			if err != nil {
				r.Logger.Error().Err(err).Str("key", key).Msg("Division by base")
				continue
			}
			totalSkips += skips

			if property == "average" {
				continue
			}
		}

		if property == "percent" {
			if skips, err = curMat.MultiplyByScalar(key, 100); err != nil {
				r.Logger.Error().Err(err).Str("key", key).Msg("Multiply by scalar")
			} else {
				totalSkips += skips
			}
			continue
		}
		// If we reach here then one of the earlier clauses should have executed `continue` statement
		r.Logger.Error().Err(err).
			Str("key", key).
			Str("property", property).
			Int("instIndex", instIndex).
			Msg("Unknown property")
	}

	// calculate rates (which we deferred to calculate averages/percents first)
	for i, metric := range orderedMetrics {
		key := orderedKeys[i]
		counter := r.counterLookup(metric, key)
		if counter != nil {
			property := counter.counterType
			if property == "rate" {
				if skips, err = curMat.Divide(orderedKeys[i], timestampMetricName); err != nil {
					r.Logger.Error().Err(err).
						Int("i", i).
						Str("metric", metric.GetName()).
						Str("key", orderedKeys[i]).
						Int("instIndex", instIndex).
						Msg("Calculate rate")
					continue
				}
				totalSkips += skips
			}
		} else {
			r.Logger.Warn().Str("counter", metric.GetName()).Msg("Counter is missing or unable to parse ")
			continue
		}
	}

	calcD := time.Since(calcStart)
	_ = r.Metadata.LazySetValueUint64("instances", "data", uint64(len(curMat.GetInstances())))
	_ = r.Metadata.LazySetValueInt64("calc_time", "data", calcD.Microseconds())
	_ = r.Metadata.LazySetValueUint64("skips", "data", uint64(totalSkips))

	// store cache for next poll
	r.Matrix[r.Object] = cachedData

	newDataMap := make(map[string]*matrix.Matrix)
	newDataMap[r.Object] = curMat
	return newDataMap, nil
}

// Poll counter "ops" of the related/parent object, required for objects
// workload_detail and workload_detail_volume. This counter is already
// collected by the other collectors, so this poll is redundant
// (until we implement some sort of inter-collector communication).
func (r *RestPerf) getParentOpsCounters(data *matrix.Matrix) error {

	var (
		ops       *matrix.Metric
		object    string
		dataQuery string
		err       error
		records   []gjson.Result
	)

	if r.Prop.Query == qosDetailQuery {
		dataQuery = path.Join(qosQuery, "rows")
		object = "qos"
	} else {
		dataQuery = path.Join(qosVolumeQuery, "rows")
		object = "qos_volume"
	}

	if ops = data.GetMetric("ops"); ops == nil {
		r.Logger.Error().Err(nil).Msgf("ops counter not found in cache")
		return errs.New(errs.ErrMissingParam, "counter ops")
	}

	var filter []string
	filter = append(filter, "counters.name=ops")
	href := rest.NewHrefBuilder().
		APIPath(dataQuery).
		Fields([]string{"*"}).
		Filter(filter).
		ReturnTimeout(r.Prop.ReturnTimeOut).
		Build()

	r.Logger.Debug().Str("href", href).Send()
	if href == "" {
		return errs.New(errs.ErrConfig, "empty url")
	}

	records, err = rest.Fetch(r.Client, href)
	if err != nil {
		r.Logger.Error().Err(err).Str("href", href).Msg("Failed to fetch data")
		return err
	}

	if len(records) == 0 {
		return errs.New(errs.ErrNoInstance, "no "+object+" instances on cluster")
	}

	for _, instanceData := range records {
		var (
			instanceKey string
			instance    *matrix.Instance
		)

		if !instanceData.IsObject() {
			r.Logger.Warn().Str("type", instanceData.Type.String()).Msg("Instance data is not object, skipping")
			continue
		}

		value := parseProperties(instanceData, "name")
		if value.Exists() {
			instanceKey += strings.Clone(value.String())
		} else {
			r.Logger.Warn().Str("key", "name").Msg("skip instance, missing key")
			continue
		}
		instance = data.GetInstance(instanceKey)
		if instance == nil {
			continue
		}

		counterName := "ops"
		f := parseMetricResponse(instanceData, counterName)
		if f.value != "" {
			if err = ops.SetValueString(instance, f.value); err != nil {
				r.Logger.Error().Err(err).Str("metric", counterName).Str("value", value.String()).Msg("set metric")
			}
		}
	}

	return nil
}

func (r *RestPerf) counterLookup(metric *matrix.Metric, metricKey string) *counter {
	var c *counter

	if metric.IsArray() {
		name, _, _ := strings.Cut(metricKey, arrayKeyToken)
		c = r.perfProp.counterInfo[name]
	} else {
		c = r.perfProp.counterInfo[metricKey]
	}
	return c
}

func (r *RestPerf) LoadPlugin(kind string, p *plugin.AbstractPlugin) plugin.Plugin {
	switch kind {
	case "Nic":
		return nic.New(p)
	case "Fcp":
		return fcp.New(p)
	case "Headroom":
		return headroom.New(p)
	case "Volume":
		return volume.New(p)
	case "VolumeTag":
		return volumetag.New(p)
	case "Disk":
		return disk.New(p)
	case "Vscan":
		return vscan.New(p)
	case "FabricPool":
		return fabricpool.New(p)
	case "FCVI":
		return fcvi.New(p)
	default:
		r.Logger.Info().Str("kind", kind).Msg("no Restperf plugin found")
	}
	return nil
}

// PollInstance updates instance cache
func (r *RestPerf) PollInstance() (map[string]*matrix.Matrix, error) {
	var (
		err     error
		records []gjson.Result
	)

	dataQuery := path.Join(r.Prop.Query, "rows")
	fields := "properties"
	var filter []string

	if isWorkloadObject(r.Prop.Query) || isWorkloadDetailObject(r.Prop.Query) {
		fields = "*"
		dataQuery = qosWorkloadQuery
		if r.Prop.Query == qosVolumeQuery || r.Prop.Query == qosDetailVolumeQuery {
			filter = append(filter, "workload_class="+r.loadWorkloadClassQuery(objWorkloadVolumeClass))
		} else {
			filter = append(filter, "workload_class="+r.loadWorkloadClassQuery(objWorkloadClass))
		}
	}

	href := rest.NewHrefBuilder().
		APIPath(dataQuery).
		Fields([]string{fields}).
		Filter(filter).
		ReturnTimeout(r.Prop.ReturnTimeOut).
		Build()

	r.Logger.Debug().Str("href", href).Send()
	if href == "" {
		return nil, errs.New(errs.ErrConfig, "empty url")
	}

	apiT := time.Now()
	r.Client.Metadata.Reset()
	records, err = rest.Fetch(r.Client, href)
	if err != nil {
		return r.handleError(err, href)
	}

	return r.pollInstance(records, time.Since(apiT))
}

func (r *RestPerf) pollInstance(records []gjson.Result, apiD time.Duration) (map[string]*matrix.Matrix, error) {
	var (
		err                              error
		oldInstances                     *set.Set
		oldSize, newSize, removed, added int
	)

	mat := r.Matrix[r.Object]
	oldInstances = set.New()
	parseT := time.Now()
	for key := range mat.GetInstances() {
		oldInstances.Add(key)
	}
	oldSize = oldInstances.Size()

	instanceKeys := r.Prop.InstanceKeys
	if isWorkloadObject(r.Prop.Query) {
		instanceKeys = []string{"uuid"}
	}
	if isWorkloadDetailObject(r.Prop.Query) {
		instanceKeys = []string{"name"}
	}

	if len(records) == 0 {
		return nil, errs.New(errs.ErrNoInstance, "no "+r.Object+" instances on cluster")
	}

	for _, instanceData := range records {
		var (
			instanceKey string
		)

		if !instanceData.IsObject() {
			r.Logger.Warn().Str("type", instanceData.Type.String()).Msg("Instance data is not object, skipping")
			continue
		}

		if isWorkloadObject(r.Prop.Query) || isWorkloadDetailObject(r.Prop.Query) {
			// The API endpoint api/storage/qos/workloads lacks an is_constituent filter, unlike qos-workload-get-iter. As a result, we must perform client-side filtering.
			// Although the api/private/cli/qos/workload endpoint includes this filter, it doesn't provide an option to fetch all records, both constituent and flexgroup types.
			if r.perfProp.disableConstituents {
				if constituentRegex.MatchString(instanceData.Get("volume").String()) {
					// skip constituent
					continue
				}
			}
		}

		// extract instance key(s)
		for _, k := range instanceKeys {
			var value gjson.Result
			if isWorkloadObject(r.Prop.Query) || isWorkloadDetailObject(r.Prop.Query) {
				value = instanceData.Get(k)
			} else {
				value = parseProperties(instanceData, k)
			}
			if value.Exists() {
				instanceKey += strings.Clone(value.String())
			} else {
				r.Logger.Warn().Str("key", k).Msg("skip instance, missing key")
				break
			}
		}

		if oldInstances.Has(instanceKey) {
			// instance already in cache
			oldInstances.Remove(instanceKey)
			instance := mat.GetInstance(instanceKey)
			r.updateQosLabels(instanceData, instance, instanceKey)
		} else if instance, err := mat.NewInstance(instanceKey); err != nil {
			r.Logger.Error().Err(err).Str("instanceKey", instanceKey).Msg("add instance")
		} else {
			r.updateQosLabels(instanceData, instance, instanceKey)
		}
	}

	for key := range oldInstances.Iter() {
		mat.RemoveInstance(key)
		r.Logger.Debug().Msgf("removed instance [%s]", key)
	}

	removed = oldInstances.Size()
	newSize = len(mat.GetInstances())
	added = newSize - (oldSize - removed)

	r.Logger.Debug().Int("new", added).Int("removed", removed).Int("total", newSize).Msg("instances")

	// update metadata for collector logs
	_ = r.Metadata.LazySetValueInt64("api_time", "instance", apiD.Microseconds())
	_ = r.Metadata.LazySetValueInt64("parse_time", "instance", time.Since(parseT).Microseconds())
	_ = r.Metadata.LazySetValueUint64("instances", "instance", uint64(newSize))
	_ = r.Metadata.LazySetValueUint64("bytesRx", "instance", r.Client.Metadata.BytesRx)
	_ = r.Metadata.LazySetValueUint64("numCalls", "instance", r.Client.Metadata.NumCalls)

	if newSize == 0 {
		return nil, errs.New(errs.ErrNoInstance, "")
	}

	return nil, err
}

func (r *RestPerf) updateQosLabels(qos gjson.Result, instance *matrix.Instance, key string) {
	if isWorkloadObject(r.Prop.Query) || isWorkloadDetailObject(r.Prop.Query) {
		for label, display := range r.perfProp.qosLabels {
			// lun,file,qtree may not always exist for workload
			if value := qos.Get(label); value.Exists() {
				instance.SetLabel(display, strings.Clone(value.String()))
			}
		}
		if r.Logger.GetLevel() == zerolog.DebugLevel {
			r.Logger.Debug().
				Str("query", r.Prop.Query).
				Str("key", key).
				Str("qos labels", dict.String(instance.GetLabels())).
				Send()
		}
	}
}

func (r *RestPerf) handleError(err error, href string) (map[string]*matrix.Matrix, error) {
	if errs.IsRestErr(err, errs.TableNotFound) || errs.IsRestErr(err, errs.APINotFound) {
		// the table or API does not exist. return ErrAPIRequestRejected so the task goes to stand-by
		return nil, fmt.Errorf("polling href=[%s] err: %w", href, errs.New(errs.ErrAPIRequestRejected, err.Error()))
	}
	return nil, fmt.Errorf("failed to fetch data. href=[%s] err: %w", href, err)
}

func isWorkloadObject(query string) bool {
	_, ok := qosQueries[query]
	return ok
}

func isWorkloadDetailObject(query string) bool {
	_, ok := qosDetailQueries[query]
	return ok
}

// Interface guards
var (
	_ collector.Collector = (*RestPerf)(nil)
)
