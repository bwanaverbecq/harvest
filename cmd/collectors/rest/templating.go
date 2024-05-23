package rest

import (
	"fmt"
	"github.com/netapp/harvest/v2/pkg/errs"
	"github.com/netapp/harvest/v2/pkg/tree/node"
	"github.com/netapp/harvest/v2/pkg/util"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (r *Rest) LoadTemplate() (string, error) {

	jitter := r.Params.GetChildContentS("jitter")
	template, path, err := r.ImportSubTemplate("", TemplateFn(r.Params, r.Object), jitter, r.Client.Cluster().Version)
	if err != nil {
		return "", err
	}

	r.Params.Union(template)
	return path, nil
}

func (r *Rest) InitCache() error {

	var (
		counters *node.Node
	)

	if x := r.Params.GetChildContentS("object"); x != "" {
		r.Prop.Object = x
	} else {
		r.Prop.Object = strings.ToLower(r.Object)
	}

	if e := r.Params.GetChildS("export_options"); e != nil {
		r.Matrix[r.Object].SetExportOptions(e)
	}

	if r.Prop.Query = r.Params.GetChildContentS("query"); r.Prop.Query == "" {
		return errs.New(errs.ErrMissingParam, "query")
	}

	// create metric cache
	if counters = r.Params.GetChildS("counters"); counters == nil {
		return errs.New(errs.ErrMissingParam, "counters")
	}

	// default value for ONTAP is 15 sec
	if returnTimeout := r.Params.GetChildContentS("return_timeout"); returnTimeout != "" {
		iReturnTimeout, err := strconv.Atoi(returnTimeout)
		if err != nil {
			r.Logger.Warn().Str("returnTimeout", returnTimeout).Msg("Invalid value of returnTimeout")
		} else {
			r.Prop.ReturnTimeOut = &iReturnTimeout
		}
	}

	// private end point do not support * as fields. We need to pass fields in endpoint
	query := r.Params.GetChildS("query")
	r.Prop.IsPublic = true
	if query != nil {
		r.Prop.IsPublic = util.IsPublicAPI(query.GetContentS())
	}

	r.ParseRestCounters(counters, r.Prop)

	r.Logger.Debug().
		Strs("extracted Instance Keys", r.Prop.InstanceKeys).
		Int("numMetrics", len(r.Prop.Metrics)).
		Int("numLabels", len(r.Prop.InstanceLabels)).
		Msg("Initialized metric cache")

	return nil
}

func HandleDuration(value string) float64 {
	// Example: duration: PT8H35M42S
	timeDurationRegex := `^P(?:(\d+)Y)?(?:(\d+)M)?(?:(\d+)D)?T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:.\d+)?)S)?$`

	regexTimeDuration := regexp.MustCompile(timeDurationRegex)
	if match := regexTimeDuration.MatchString(value); match {
		// example: PT8H35M42S   ==>  30942
		matches := regexTimeDuration.FindStringSubmatch(value)
		if matches == nil {
			return 0
		}

		seconds := 0.0

		// years
		// months

		// days
		if matches[3] != "" {
			f, err := strconv.ParseFloat(matches[3], 64)
			if err != nil {
				fmt.Printf("%v", err)
				return 0
			}
			seconds += f * 24 * 60 * 60
		}

		// hours
		if matches[4] != "" {
			f, err := strconv.ParseFloat(matches[4], 64)
			if err != nil {
				fmt.Printf("%v", err)
				return 0
			}
			seconds += f * 60 * 60
		}

		// minutes
		if matches[5] != "" {
			f, err := strconv.ParseFloat(matches[5], 64)
			if err != nil {
				fmt.Printf("%v", err)
				return 0
			}
			seconds += f * 60
		}

		// seconds & milliseconds
		if matches[6] != "" {
			f, err := strconv.ParseFloat(matches[6], 64)
			if err != nil {
				fmt.Printf("%v", err)
				return 0
			}
			seconds += f
		}
		return seconds
	}

	return 0
}

// Example: timestamp: 2020-12-02T18:36:19-08:00
var regexTimeStamp = regexp.MustCompile(
	`[+-]?\d{4}(-[01]\d(-[0-3]\d(T[0-2]\d:[0-5]\d:?([0-5]\d(\.\d+)?)?[+-][0-2]\d:[0-5]\d?)?)?)?`)

func HandleTimestamp(value string) float64 {
	var timestamp time.Time
	var err error

	if match := regexTimeStamp.MatchString(value); match {
		// example: 2020-12-02T18:36:19-08:00   ==>  1606962979
		if timestamp, err = time.Parse(time.RFC3339, value); err != nil {
			fmt.Printf("%v", err)
			return 0
		}
		return float64(timestamp.Unix())
	}
	return 0
}

func (r *Rest) ParseRestCounters(counter *node.Node, prop *prop) {
	var (
		display, name, kind, metricType string
	)

	instanceKeys := make(map[string]string)

	for _, c := range counter.GetAllChildContentS() {
		if c != "" {
			name, display, kind, metricType = util.ParseMetric(c)
			prop.Counters[name] = display
			switch kind {
			case "key":
				prop.InstanceLabels[name] = display
				instanceKeys[display] = name
			case "label":
				prop.InstanceLabels[name] = display
			case "float":
				m := &Metric{Label: display, Name: name, MetricType: metricType, Exportable: true}
				prop.Metrics[name] = m
			}
		}
	}

	// populate prop.instanceKeys
	// sort keys by display name. This is needed to match counter and endpoints keys
	keys := util.GetSortedKeys(instanceKeys)

	// Append instance keys to prop
	for _, k := range keys {
		prop.InstanceKeys = append(prop.InstanceKeys, instanceKeys[k])
	}

	counterKey := make([]string, len(prop.Counters))
	i := 0
	for k := range prop.Counters {
		counterKey[i] = k
		i++
	}
	prop.Fields = counterKey
	if counter != nil {
		if x := counter.GetChildS("filter"); x != nil {
			prop.Filter = append(prop.Filter, x.GetAllChildContentS()...)
		}
	}

	if prop.IsPublic {
		if counter != nil {
			if x := counter.GetChildS("hidden_fields"); x != nil {
				prop.HiddenFields = append(prop.HiddenFields, x.GetAllChildContentS()...)
			}
		}
	}

}
