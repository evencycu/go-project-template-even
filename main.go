package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"gitlab.com/cake/go-project-template/apiserver"
	"gitlab.com/cake/go-project-template/gpt"
	"gitlab.com/cake/goctx"
	"gitlab.com/cake/gotrace/v2"
	"gitlab.com/cake/m800log"
	"gitlab.com/cake/mgopool"

	"github.com/spf13/viper"
	jaeger "github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
)

var configFile string
var systemCtx goctx.Context

func init() {
	flag.StringVar(&configFile, "config", "./local.toml", "Path to Config File")

	systemCtx = goctx.Background()
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [arguments] <command> \n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	viper.AutomaticEnv()
	viper.SetConfigFile(configFile)
	viper.ReadInConfig() // Find and read the config file

	replacer := strings.NewReplacer(".", "_")
	viper.SetEnvKeyReplacer(replacer)

	if viper.GetBool("app.prof") {
		ActivateProfile()
	}

	// Init log
	log.SetFlags(log.LstdFlags)
	err := m800log.Initialize(viper.GetString("log.output"), viper.GetString("log.level"))
	if err != nil {
		panic(err)
	}
	m800log.SetM800JSONFormatter(viper.GetString("log.timestamp_format"), gpt.GetAppName(), gpt.GetVersion().Version, gpt.GetPhaseEnv(), gpt.GetNamespace())
	m800log.SetAccessLevel(viper.GetString("log.access_level"))
	// Init tracer
	closer, err := initTracer()
	if err != nil {
		panic(err)
	}
	if closer != nil {
		defer closer.Close()
	}

	httpServer := apiserver.InitGinServer(systemCtx)

	// Init mongo
	// if connect to multi mongodb cluster, take this pool to use

	// pool, err = mgopool.NewSessionPool(getXXXMongoDBInfo())
	// if err != nil {
	// 	m800log.Error(systemCtx, "mongo connect error:", err, ", config:", viper.AllSettings())
	// }

	err = mgopool.Initialize(getMongoDBInfo())
	if err != nil {
		m800log.Error(systemCtx, "mongo connect error:", err, ", config:", viper.AllSettings())
		panic(err)
	}

	// graceful shutdown
	quit := make(chan os.Signal, 5)
	signal.Notify(quit, os.Interrupt)
	<-quit
	log.Println("Shutdown Server ...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatal("Server Shutdown error:", err)
	}
	log.Println("Server exiting")
}

func initTracer() (io.Closer, error) {
	if !viper.GetBool("jaeger.enabled") {
		log.Println("Jaeger disabled")
		return nil, nil
	}
	sConf := &jaegercfg.SamplerConfig{
		Type:  jaeger.SamplerTypeRateLimiting,
		Param: viper.GetFloat64("jaeger.sample_rate"),
	}
	rConf := &jaegercfg.ReporterConfig{
		QueueSize:           viper.GetInt("jaeger.queue_size"),
		BufferFlushInterval: viper.GetDuration("jaeger.flush_interval"),
		LocalAgentHostPort:  viper.GetString("jaeger.host"),
		LogSpans:            viper.GetBool("jaeger.log_spans"),
	}
	log.Printf("Sampler Config:%+v\nReporterConfig:%+v\n", sConf, rConf)
	closer, err := gotrace.InitJaeger(gpt.GetAppName(), sConf, rConf)
	if err != nil {
		return nil, fmt.Errorf("init tracer error:%s", err.Error())
	}
	return closer, nil
}

func getMongoDBInfo() *mgopool.DBInfo {
	name := viper.GetString("database.mgo.name")
	mgoMaxConn := viper.GetInt("database.mgo.max_conn")
	mgoUser := viper.GetString("database.mgo.user")
	mgoPassword := viper.GetString("database.mgo.password")
	mgoAuthDatabase := viper.GetString("database.mgo.authdatabase")
	mgoTimeout := viper.GetDuration("database.mgo.timeout")
	mgoDirect := viper.GetBool("database.mgo.direct")
	mgoSecondary := viper.GetBool("database.mgo.secondary")
	mgoMonogs := viper.GetBool("database.mgo.mongos")
	mgoAddrs := strings.Split(viper.GetString("database.mgo.hosts"), ";")
	if len(mgoAddrs) == 0 {
		log.Fatal("Config error: no mongo hosts")
	}
	return mgopool.NewDBInfo(name, mgoAddrs, mgoUser, mgoPassword,
		mgoAuthDatabase, mgoTimeout, mgoMaxConn, mgoDirect, mgoSecondary, mgoMonogs)
}
