package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	argoexecutor "github.com/argoproj/argo-workflows/v3/pkg/plugins/executor"
	"github.com/google/uuid"
	"io"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	apiVersion               = "v1"
	keptnMetricsResourceName = "keptnmetrics"
	keptnTasksResourceName   = "keptntasks"
	analysisResourceName     = "analyses"

	paramKeptnQuery = "query"
	paramStart      = "start"
	paramEnd        = "end"
)

type taskStatus struct {
	createdResource schema.GroupVersionResource
	name            string
	namespace       string
}

var keptnMetricsResource = schema.GroupVersionResource{
	Group:    "metrics.keptn.sh",
	Version:  apiVersion,
	Resource: keptnMetricsResourceName,
}

var analysisResource = schema.GroupVersionResource{
	Group:    "metrics.keptn.sh",
	Version:  apiVersion,
	Resource: analysisResourceName,
}

var taskResource = schema.GroupVersionResource{
	Group:    "lifecycle.keptn.sh",
	Version:  apiVersion,
	Resource: keptnTasksResourceName,
}

type queryObject struct {
	GroupVersionResource schema.GroupVersionResource
	ResourceName         string
	DurationString       string
	Start                string
	End                  string
	Namespace            string
	Arguments            map[string]interface{}
}

type queryResult struct {
	Details string
	Result  string
	Start   string
	End     string
	Status  *taskStatus
	Requeue bool
}

type Executor struct {
	token           string
	client          dynamic.Interface
	analysisTimeout time.Duration
	taskTimeout     time.Duration

	workflowExecutions map[string]*taskStatus
}

func New(cfg *rest.Config, token string) (*Executor, error) {
	if cfg == nil {
		return nil, errors.New("could not initialize Executor: no KubeConfig provided")
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("could not initialize Executor: %w", err)
	}
	return &Executor{
		token:              token,
		client:             client,
		analysisTimeout:    15 * time.Second,
		taskTimeout:        5 * time.Minute,
		workflowExecutions: map[string]*taskStatus{},
	}, nil
}

func (e *Executor) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	if e.token != "" {
		if r.Header.Get("Authorization") != "Bearer "+e.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Log(r.Context(), slog.LevelWarn, fmt.Sprintf("decoding error: %v", err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	slog.Log(r.Context(), slog.LevelInfo, string(body))

	templateArgs := &argoexecutor.ExecuteTemplateArgs{}

	err = json.Unmarshal(body, templateArgs)
	if err != nil {
		slog.Log(r.Context(), slog.LevelWarn, fmt.Sprintf("unmarshalling error: %v", err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// get the parameters

	query := ""
	start := ""
	end := ""
	for _, param := range templateArgs.Template.Inputs.Parameters {
		if param.Name == paramKeptnQuery {
			query = param.GetValue()
		}
		if param.Name == paramStart {
			start = param.GetValue()
		}
		if param.Name == paramEnd {
			end = param.GetValue()
		}
	}
	if query == "" {
		slog.Log(r.Context(), slog.LevelWarn, fmt.Sprintf("received no query in parameters"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	queryObj, err := parseQuery(query)
	if err != nil {
		slog.Log(r.Context(), slog.LevelWarn, fmt.Sprintf("could not parse query: %v", err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	queryObj.Start = start
	queryObj.End = end

	var res *queryResult
	switch queryObj.GroupVersionResource.Resource {
	case keptnMetricsResourceName:
		res, err = e.queryKeptnMetric(queryObj)
		if err != nil {
			slog.Log(r.Context(), slog.LevelError, fmt.Sprintf("Could not execute KeptnMetric query: %v", err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

	case analysisResourceName:
		res, err = e.queryKeptnAnalysis(queryObj)
		if err != nil {
			slog.Log(r.Context(), slog.LevelError, fmt.Sprintf("Could not execute Analysis query: %v", err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	case keptnTasksResourceName:
		if status, ok := e.workflowExecutions[templateArgs.Workflow.ObjectMeta.Uid]; ok {
			res, err = e.checkForTaskStatus(queryObj, context.Background(), status.name, status.namespace)
		} else {
			res, err = e.executeKeptnTask(queryObj)
		}
		if err != nil {
			slog.Log(r.Context(), slog.LevelError, fmt.Sprintf("Could not execute Analysis query: %v", err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var reply *argoexecutor.ExecuteTemplateReply
	if res.Requeue {
		reply = &argoexecutor.ExecuteTemplateReply{
			Node: &wfv1.NodeResult{
				Phase:    wfv1.NodeRunning,
				Message:  fmt.Sprintf("Executing %s", query),
				Progress: wfv1.Progress("0/1"),
			},
			Requeue: &v1.Duration{
				Duration: 5 * time.Second,
			},
		}
		if res.Status != nil {
			e.workflowExecutions[templateArgs.Workflow.ObjectMeta.Uid] = res.Status
		}
	} else {
		reply = &argoexecutor.ExecuteTemplateReply{
			Node: &wfv1.NodeResult{
				Phase:   wfv1.NodeSucceeded,
				Message: fmt.Sprintf("Result of %s: %s", query, res.Result),
				Outputs: &wfv1.Outputs{
					Parameters: []wfv1.Parameter{
						{
							Name:  "result",
							Value: wfv1.AnyStringPtr(res.Result),
						},
						{
							Name:  "details",
							Value: wfv1.AnyStringPtr(res.Details),
						},
						{
							Name:  "start",
							Value: wfv1.AnyStringPtr(res.Start),
						},
						{
							Name:  "end",
							Value: wfv1.AnyStringPtr(res.End),
						},
					},
					Result: &res.Details,
				},
				Progress: wfv1.Progress("1/1"),
			},
		}
	}

	marshal, err := json.Marshal(reply)

	if err != nil {
		slog.Log(r.Context(), slog.LevelError, fmt.Sprintf("Could not marshal result: %v", err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(marshal)
	if err != nil {
		slog.Log(r.Context(), slog.LevelError, fmt.Sprintf("Could not marshal result: %v", err))
	}
}

func (e *Executor) queryKeptnAnalysis(obj *queryObject) (*queryResult, error) {

	timeFrame := map[string]string{}

	if obj.Start != "" && obj.End != "" {
		timeFrame["from"] = obj.Start
		timeFrame["to"] = obj.End
	} else {
		timeFrame["recent"] = obj.DurationString
	}
	analysis := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("metrics.keptn.sh/%s", apiVersion),
			"kind":       "Analysis",
			"metadata": map[string]interface{}{
				"name":      fmt.Sprintf("%s-%s", obj.ResourceName, uuid.New().String()[:6]),
				"namespace": obj.Namespace,
			},
			"spec": map[string]interface{}{
				"analysisDefinition": map[string]interface{}{
					"name": obj.ResourceName,
				},
				"timeframe": timeFrame,
				"args":      obj.Arguments,
			},
		},
	}

	// set the timeout to 10s - this will give Keptn enough time to reconcile the Analysis
	// and store the result in the status of the resource created here.
	ctx, cancel := context.WithTimeout(context.Background(), e.analysisTimeout)
	defer cancel()

	createdAnalysis, err := e.client.
		Resource(analysisResource).
		Namespace(obj.Namespace).
		Create(ctx, analysis, v1.CreateOptions{})

	if err != nil {
		return nil, fmt.Errorf("could not create Keptn Analysis %s/%s: %w", obj.Namespace, obj.ResourceName, err)
	}

	// delete the created analysis at the end of the function
	defer func() {
		_ = e.client.
			Resource(analysisResource).
			Namespace(obj.Namespace).
			Delete(
				context.TODO(),
				createdAnalysis.GetName(),
				v1.DeleteOptions{},
			)
	}()

	for {
		// retrieve the current state of the created Analysis resource every 1s, until
		// it has been completed, and the evaluation result is available.
		// We do this until the timeout of the context expires. If no result is available
		// by then, we return an error.
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("encountered timeout while waiting for Keptn Analysis %s/%s to be finished", obj.Namespace, obj.ResourceName)
		case <-time.After(500 * time.Millisecond):
			get, err := e.client.Resource(analysisResource).Namespace(obj.Namespace).Get(ctx, createdAnalysis.GetName(), v1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("could not check status of created Keptn Analysis %s/%s: %w", obj.Namespace, obj.ResourceName, err)
			}
			statusStr, ok, err := unstructured.NestedString(get.Object, "status", "state")
			if err != nil {
				return nil, fmt.Errorf("could not check status of created Keptn Analysis %s/%s: %w", obj.Namespace, obj.ResourceName, err)
			}
			if ok && statusStr == "Completed" {
				res := &queryResult{}
				if passed, ok, _ := unstructured.NestedBool(get.Object, "status", "pass"); ok && passed {
					res.Result = "pass"
				} else if warning, ok, _ := unstructured.NestedBool(get.Object, "status", "warning"); ok && warning {
					res.Result = "warning"
				}

				if raw, ok, _ := unstructured.NestedString(get.Object, "status", "raw"); ok {
					res.Details = raw
				}
				return res, nil
			}
		}
	}
}

func (e *Executor) queryKeptnMetric(queryObj *queryObject) (*queryResult, error) {
	get, err := e.client.Resource(queryObj.GroupVersionResource).
		Namespace(queryObj.Namespace).
		Get(
			context.Background(),
			queryObj.ResourceName,
			v1.GetOptions{},
		)

	if err != nil {
		return nil, fmt.Errorf("could not retrieve KeptnMetric %s/%s: %w", queryObj.Namespace, queryObj.ResourceName, err)
	}

	if status, ok := get.Object["status"]; ok {
		if statusObj, ok := status.(map[string]interface{}); ok {
			if value, ok := statusObj["value"].(string); ok {
				return &queryResult{
					Result: value,
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("could not retrieve KeptnMetric - no value found in resource %s/%s", queryObj.Namespace, queryObj.ResourceName)
}

func (e *Executor) executeKeptnTask(obj *queryObject) (*queryResult, error) {
	task := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("lifecycle.keptn.sh/%s", apiVersion),
			"kind":       "KeptnTask",
			"metadata": map[string]interface{}{
				"name":      fmt.Sprintf("%s-%s", obj.ResourceName, uuid.New().String()[:6]),
				"namespace": obj.Namespace,
			},
			"spec": map[string]interface{}{
				"taskDefinition": obj.ResourceName,
				"parameters":     obj.Arguments,
				// these context attributes are required
				"context": map[string]string{
					"appName":         "",
					"appVersion":      "",
					"objectType":      "",
					"taskType":        "",
					"workloadName":    "",
					"workloadVersion": "",
				},
			},
		},
	}

	// set the timeout to 10s - this will give Keptn enough time to reconcile the Analysis
	// and store the result in the status of the resource created here.
	ctx, cancel := context.WithTimeout(context.Background(), e.taskTimeout)
	defer cancel()

	createdTask, err := e.client.
		Resource(taskResource).
		Namespace(obj.Namespace).
		Create(ctx, task, v1.CreateOptions{})

	if err != nil {
		return nil, fmt.Errorf("could not create Keptn Analysis %s/%s: %w", obj.Namespace, obj.ResourceName, err)
	}

	res := &queryResult{
		Requeue: true,
		Status: &taskStatus{
			createdResource: taskResource,
			name:            createdTask.GetName(),
			namespace:       createdTask.GetNamespace(),
		},
	}
	return res, nil

}

func (e *Executor) checkForTaskStatus(obj *queryObject, ctx context.Context, name, namespace string) (*queryResult, error) {

	get, err := e.client.Resource(taskResource).Namespace(namespace).Get(ctx, name, v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not check status of created Keptn Task %s/%s: %w", obj.Namespace, obj.ResourceName, err)
	}
	// check completion by checking end time
	endTimeStr, ok, err := unstructured.NestedString(get.Object, "status", "endTime")
	if ok && endTimeStr != "" {
		startTimeStr, _, err := unstructured.NestedString(get.Object, "status", "startTime")
		if err != nil {
			return nil, fmt.Errorf("could not check status of created Keptn Task %s/%s: %w", obj.Namespace, obj.ResourceName, err)
		}
		statusStr, _, err := unstructured.NestedString(get.Object, "status", "status")
		if err != nil {
			return nil, fmt.Errorf("could not check status of created Keptn Task %s/%s: %w", obj.Namespace, obj.ResourceName, err)
		}
		res := &queryResult{
			Result: statusStr,
			Start:  startTimeStr,
			End:    endTimeStr,
		}
		return res, nil
	}
	return &queryResult{
		Requeue: true,
	}, nil
}

func parseQuery(query string) (*queryObject, error) {
	result := &queryObject{}
	// sanitize the query by converting to lower case, trimming spaces and line break characters
	split := strings.Split(
		strings.TrimSpace(
			strings.TrimSuffix(
				strings.ToLower(query),
				"\n",
			),
		),
		"/",
	)

	if len(split) < 3 {
		return nil, errors.New("unexpected query format. query must be in the format <keptnmetric|analysis>/<namespace>/<resourceName>/<duration>/<arguments>")
	}
	switch split[0] {
	// take into account both singular and plural naming of resource names, to reduce probability of errors
	case "keptnmetric", keptnMetricsResourceName:
		result.GroupVersionResource = keptnMetricsResource
		break
	case "analysis", analysisResourceName:
		result.GroupVersionResource = analysisResource
		// add the duration for the Analysis, if available
		if len(split) >= 4 {
			result.DurationString = split[3]
		} else {
			//set to '1m' by default
			result.DurationString = "1m"
		}

		// add arguments - these are provided as a comma separated list of key/value pairs
		result.Arguments = map[string]interface{}{}
		if len(split) >= 5 {
			args := strings.Split(split[4], ";")

			for i := 0; i < len(args); i++ {
				keyValue := strings.Split(args[i], "=")
				if len(keyValue) == 2 {
					result.Arguments[keyValue[0]] = keyValue[1]
				}
			}
		}
	case "keptntask", keptnTasksResourceName:
		result.GroupVersionResource = taskResource

		if len(split) >= 4 {
			args := strings.Split(split[3], ";")

			for i := 0; i < len(args); i++ {
				keyValue := strings.Split(args[i], "=")
				if len(keyValue) == 2 {
					result.Arguments[keyValue[0]] = keyValue[1]
				}
			}
		}
		break

	default:
		return nil, errors.New("unexpected resource kind provided in the query. must be one of: ['keptnmetric', 'analysis']")
	}

	result.Namespace = split[1]
	result.ResourceName = split[2]

	return result, nil
}
