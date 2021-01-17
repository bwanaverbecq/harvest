package main

import (
    "fmt"
    "log"
    "path/filepath"
    "local.host/params"
    "local.host/template"
	"local.host/collector"
	"local.host/exporter"
    "local.host/matrix"
    "local.host/logger"
)


func main() {

    var err error
    var data *matrix.Matrix
    var p params.Params
    var t *template.Element
    var c *collector.Collector
    var e *exporter.Exporter

	p = params.NewFromArgs()

    fmt.Println(p.Path)
    fmt.Printf("handler output: (%T) %v\n", log.Writer(), log.Writer() )

    err = logger.OpenFileOutput(p.Path, "goharvest2_poller.log")
    //logf, err = os.OpenFile(logfp, os.O_APPEND | os.O_CREATE | os.O_WRONLY, 0644)
    if err != nil {
        panic(err)
    }
    defer logger.CloseFileOutput()
    fmt.Printf("handler output: (%T) %v\n", log.Writer(), log.Writer() )

    //log.SetOutput(logf)
    //log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile | log.Lmsgprefix)

    Log := logger.New(1, "")
    Log.Info("Opened logger file, starting-up Poller")

    fmt.Printf("handler output: (%T) %v\n", log.Writer(), log.Writer() )

    t, err = template.New(filepath.Join(p.Path, "var/zapi/", p.Template+".yaml" ))
    if err != nil { panic(err) }

    t.PrintTree(0)

    fmt.Printf("t = %v (%T)\n", t, t)
    fmt.Printf("&t = %v (%T)\n", &t, &t)
    fmt.Printf("*t = %v (%T)\n", *t, *t)

    Log.Debug("Loaded Parameters and Template. Starting up collectors and exporters....")

    c = collector.New("Zapi", p.Object)
    fmt.Printf("c = %v (%T)\n", c, c)
    fmt.Printf("&c = %v (%T)\n", &c, &c)
    fmt.Printf("*c = %v (%T)\n", *c, *c)

    err = c.Init(p, t)
    if err != nil { panic(err) }

    err = c.PollInstance()
    if err != nil { panic(err) }

    data, err = c.PollData()
    if err != nil { panic(err) }

    e = exporter.New("Prometheus", "pandro")
    err = e.Init()
    if err != nil { panic(err) }

    err = e.Export(data, c.Template.GetChild("export_options"))
    if err != nil { panic(err) }
    fmt.Println("SUCCESS")

    data.Print()
    Log.Info("Cleaning up and shutting down Poller")
}
