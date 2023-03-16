package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"text/template"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1"
	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/GoogleCloudPlatform/cloud-build-notifiers/lib/notifiers"
	log "github.com/golang/glog"
	// deprecated "google.golang.org/genproto/googleapis/devtools/cloudbuild/v1"
)

const (
	githubTokenSecretName = "githubToken"
	githubApiEndpoint     = "https://api.github.com/repos"
)

func main() {
	if err := notifiers.Main(new(githubdeploymentsNotifier)); err != nil {
		log.Fatalf("fatal error: %v", err)
	}
}

type githubdeploymentsNotifier struct {
	filter               notifiers.EventFilter
	githubToken          string
	deploymentTmpl       *template.Template
	deploymentStatusTmpl *template.Template

	br       notifiers.BindingResolver
	tmplView *notifiers.TemplateView

	cloudbuildClient *cloudbuild.Client
}

const deploymentPayload = `{
 	"environment": "{{.Build.Substitutions._ENVIRONMENT}}",
    "ref": "{{.Build.Substitutions.RefName}}",
    "description": "Cloud Build {{.Build.ProjectId}} {{.Build.Id}} status: **{{.Build.Status}}**\n\n{{if .Build.BuildTriggerId}}Trigger ID: {{.Build.BuildTriggerId}}{{end}}\n\n[View Logs]({{.Build.LogUrl}})"
    "payload": "{}",
	"task": "",
}`

const deploymentStatusPayload = `{
 	"environment": "{{.Build.Substitutions._ENVIRONMENT}}",
    "state": "{{.Params.Status}}",
    "description": "Cloud Build {{.Build.ProjectId}} {{.Build.Id}} status: **{{.Build.Status}}**\n\n{{if .Build.BuildTriggerId}}Trigger ID: {{.Build.BuildTriggerId}}{{end}}\n\n[View Logs]({{.Build.LogUrl}})"
	"log_url": "{{.Build.LogUrl}}",
	"environment_url": "{{.Build.Substitutions._ENVIRONMENT_URL}}"
}`

type githubdeploymentsInitMessage struct {
	Environment string `json:"environment"`
	Ref         string `json:"ref"`
	Description string `json:"description"`
	Payload     string `json:"payload"`
	Task        string `json:"task"`
}

type githubdeploymentsUpdateMessage struct {
	Environment    string `json:"environment"`
	State          string `json:"state"`
	Description    string `json:"description"`
	LogUrl         string `json:"log_url"`
	EnvironmentUrl string `json:"environment_url"`
}

func (g *githubdeploymentsNotifier) SetUp(ctx context.Context, cfg *notifiers.Config, _ string, sg notifiers.SecretGetter, br notifiers.BindingResolver) error {
	prd, err := notifiers.MakeCELPredicate(cfg.Spec.Notification.Filter)
	if err != nil {
		return fmt.Errorf("failed to make a CEL predicate: %w", err)
	}
	g.filter = prd
	g.br = br

	cloudbuildClient, err := cloudbuild.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize Cloud Build client: %w", err)
	}
	g.cloudbuildClient = cloudbuildClient

	deploymentTmpl, err := template.New("deployment_template").Parse(deploymentPayload)
	if err != nil {
		return fmt.Errorf("failed to parse deployment body template: %w", err)
	}
	g.deploymentTmpl = deploymentTmpl

	deploymentStatusTmpl, err := template.New("deployment_status_template").Parse(deploymentStatusPayload)
	if err != nil {
		return fmt.Errorf("failed to parse deployment body template: %w", err)
	}
	g.deploymentStatusTmpl = deploymentStatusTmpl

	wuRef, err := notifiers.GetSecretRef(cfg.Spec.Notification.Delivery, githubTokenSecretName)
	if err != nil {
		return fmt.Errorf("failed to get Secret ref from delivery config (%v) field %q: %w", cfg.Spec.Notification.Delivery, githubTokenSecretName, err)
	}
	wuResource, err := notifiers.FindSecretResourceName(cfg.Spec.Secrets, wuRef)
	if err != nil {
		return fmt.Errorf("failed to find secret for ref %q: %w", wuRef, err)
	}
	wu, err := sg.GetSecret(ctx, wuResource)
	if err != nil {
		return fmt.Errorf("failed to get token secret: %w", err)
	}
	g.githubToken = wu

	return nil
}

func (g *githubdeploymentsNotifier) SendNotification(ctx context.Context, build *cloudbuildpb.Build) error {
	log.V(1).Infof("[DEBUG] at SendNotification: build=%+v", build)

	if !g.filter.Apply(ctx, build) {
		log.V(2).Infof("not sending response for event (build id = %s, status = %v)", build.Id, build.Status)
		return nil
	}

	if build.BuildTriggerId == "" {
		log.Warningf("build passes filter but does not have a trigger ID. Build id: %q, status: %v", build.Id, build.GetStatus())
		return nil
	}

	getTriggerReq := &cloudbuildpb.GetBuildTriggerRequest{ProjectId: build.GetProjectId(), TriggerId: build.GetBuildTriggerId()}
	triggerInfo, err := g.cloudbuildClient.GetBuildTrigger(ctx, getTriggerReq)
	if err != nil {
		return fmt.Errorf("failed to get Build Trigger info: %w", err)
	}
	if triggerInfo.GetGithub() == nil {
		log.V(2).Infof("Skipped due to build trigger without github connection settings")
		log.V(2).Infof("not sending response for event (build id = %s, status = %v)", build.Id, build.Status)
		return nil
	}

	owner := triggerInfo.GetGithub().GetOwner()
	repo := triggerInfo.GetGithub().GetName()
	sha := build.Substitutions["COMMIT_SHA"]

	var webhookURL string
	var tmpl *template.Template
	deploymentStatus := toGitHubDeploymentStatus(build.Status)

	if build.Status == cloudbuildpb.Build_QUEUED {
		webhookURL = fmt.Sprintf("%s/%s/%s/deployments", githubApiEndpoint, owner, repo)
		tmpl = g.deploymentTmpl
	} else {
		deploymentId, err := g.getDeploymentId(ctx, owner, repo, sha)
		if err != nil {
			return fmt.Errorf("failed to get deployment_id: owner=%s, repo=%s, sha=%s", owner, repo, sha)
		}
		webhookURL = fmt.Sprintf("%s/%s/%s/deployments/%d", githubApiEndpoint, owner, repo, deploymentId)
		tmpl = g.deploymentStatusTmpl
	}

	log.Infof("sending GitHub Deployment webhook for Build %q (status: %q) to url %q", build.Id, build.Status, webhookURL)

	bindings, err := g.br.Resolve(ctx, nil, build)
	if err != nil {
		log.Errorf("failed to resolve bindings: %v", err)
	}
	bindings["State"] = deploymentStatus

	g.tmplView = &notifiers.TemplateView{
		Build:  &notifiers.BuildView{Build: build},
		Params: bindings,
	}
	logURL, err := notifiers.AddUTMParams(build.LogUrl, notifiers.HTTPMedium)
	if err != nil {
		return fmt.Errorf("failed to add UTM params: %w", err)
	}
	build.LogUrl = logURL

	payload := new(bytes.Buffer)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, g.tmplView); err != nil {
		return err
	}
	err = json.NewEncoder(payload).Encode(buf)
	if err != nil {
		return fmt.Errorf("failed to encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(buf.String()))
	if err != nil {
		return fmt.Errorf("failed to create a new HTTP request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Authorization", fmt.Sprintf("token %s", g.githubToken))
	req.Header.Set("User-Agent", "GCB-Notifier/0.1 (http)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warningf("got a non-OK response status %q (%d) from %q", resp.Status, resp.StatusCode, webhookURL)
	}

	b, err := httputil.DumpResponse(resp, true)
	if err != nil {
		return fmt.Errorf("failed to dump http response")
	}
	log.V(1).Infof("github api response: %q", string(b))

	log.V(2).Infoln("send HTTP request successfully")
	return nil
}

func (g *githubdeploymentsNotifier) getDeploymentId(ctx context.Context, owner, repo, sha string) (int, error) {
	webhookURL := fmt.Sprintf("%s/%s/%s/deployments", githubApiEndpoint, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webhookURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to make request: url=%q, %w", webhookURL, err)
	}
	q := req.URL.Query()
	q.Add("sha", sha)
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Authorization", fmt.Sprintf("token %s", g.githubToken))
	req.Header.Set("User-Agent", "GCB-Notifier/0.1 (http)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to make HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warningf("got a non-OK response status %q (%d) from %q", resp.Status, resp.StatusCode, webhookURL)
		return 0, fmt.Errorf("failed to call list deployments api: response status=%q, url=%q", resp.Status, webhookURL)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response body: %w", err)
	}

	log.Infof("matched deployments: %+v", string(respBody))

	type Deployment struct {
		Url string `json:"url"`
		ID  int    `json:"id"`
	}

	var deployments []Deployment
	if err := json.Unmarshal(respBody, &deployments); err != nil {
		return 0, fmt.Errorf("failed to unmarshall response body: %w", err)
	}

	if len(deployments) == 0 {
		return 0, fmt.Errorf("deployment not found: repo=%s/%s, sha=%s", owner, repo, sha)
	}

	var deploymentID int
	for _, d := range deployments {
		if deploymentID <= d.ID {
			deploymentID = d.ID
		}
	}

	return deploymentID, nil
}

func toGitHubDeploymentStatus(status cloudbuildpb.Build_Status) string {
	stateMap := map[cloudbuildpb.Build_Status]string{
		cloudbuildpb.Build_PENDING:        "pending",
		cloudbuildpb.Build_WORKING:        "in_progress",
		cloudbuildpb.Build_SUCCESS:        "success",
		cloudbuildpb.Build_FAILURE:        "failure",
		cloudbuildpb.Build_TIMEOUT:        "error",
		cloudbuildpb.Build_INTERNAL_ERROR: "error",
		cloudbuildpb.Build_CANCELLED:      "error",
		cloudbuildpb.Build_EXPIRED:        "error",
	}

	deploymentStatus, ok := stateMap[status]
	if !ok {
		return ""
	}

	return deploymentStatus
}
