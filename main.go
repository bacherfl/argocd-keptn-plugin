package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/bacherfl/argo-keptn-plugin/pkg/executor"
	"github.com/gorilla/mux"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	ctx := context.Background()

	// the port number must not be one of the common port numbers, according to
	// https://argo-workflows.readthedocs.io/en/latest/executor_plugins/
	port := 1370

	token, err := os.ReadFile("/var/run/argo/token")
	if err != nil {
		// disable authentication if no token is available
		slog.Log(ctx, slog.LevelWarn, "No token found in '/var/run/argo/token'. Will not authenticate requests")
	}

	//var kubeconfig *string
	//if home := homedir.HomeDir(); home != "" {
	//	kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	//} else {
	//	kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	//}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		panic(err.Error())
	}

	exctr, err := executor.New(config, string(token))
	if err != nil {
		log.Fatalf("Could not initialise executor: %v", err)
	}

	r := mux.NewRouter()

	slog.Log(ctx, slog.LevelInfo, fmt.Sprintf("starting server on port: %v", port))

	r.Handle("/api/v1/template.execute", exctr).Methods("POST")

	err = http.ListenAndServe(fmt.Sprintf(":%d", port), r)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Log(ctx, slog.LevelError, "server error: %v", err.Error())
	}
}
