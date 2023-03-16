package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1"
	cloudbuildpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
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
	filter      notifiers.EventFilter
	githubToken string
	createTmpl  *template.Template
	updateTmpl  *template.Template

	br       notifiers.BindingResolver
	tmplView *notifiers.TemplateView

	cloudbuildClient *cloudbuild.Client
}

const createDeploymentTemplateBody = `{
    "ref": "{{.Build.RefName}}",
    "environment": "{{.Build.Environment}}",
    "payload": "{}",
    "description": "Cloud Build {{.Build.ProjectId}} {{.Build.Id}} status: **{{.Build.Status}}**\n\n{{if .Build.BuildTriggerId}}Trigger ID: {{.Build.BuildTriggerId}}{{end}}\n\n[View Logs]({{.Build.LogUrl}})"
}`

const updateDeploymentTemplateBody = `{
    "state": "{{.Build.State}}",
    "target_url": "{{.Build.TargetUrl}}",
    "log_url": "{{.Build.LogUrl}}",
    "description": "{{.Build.Description}}",
    "environment_url": "{{.Build.EnvironmentUrl}}"
}`

type githubdeploymentsInitMessage struct {
	Environment string `json:"environment"`
	Ref         string `json:"ref"`
	Payload     string `json:"payload"`
	Description string `json:"description"`
}

type githubdeploymentsUpdateMessage struct {
	State          string `json:"state"`
	TargetUrl      string `json:"target_url"`
	LogUrl         string `json:"log_url"`
	Description    string `json:"description"`
	Environment    string `json:"environment"`
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

	createTmpl, err := template.New("create_template").Parse(createDeploymentTemplateBody)
	if err != nil {
		return fmt.Errorf("failed to parse deployment body template: %w", err)
	}
	g.createTmpl = createTmpl

	updateTmpl, err := template.New("update_template").Parse(updateDeploymentTemplateBody)
	if err != nil {
		return fmt.Errorf("failed to parse deployment body template: %w", err)
	}
	g.updateTmpl = updateTmpl

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
	if !g.filter.Apply(ctx, build) {
		log.V(2).Infof("not sending response for event (build id = %s, status = %v)", build.Id, build.Status)
		return nil
	}

	getTriggerReq := &cloudbuildpb.GetBuildTriggerRequest{
		ProjectId: build.GetProjectId(),
		TriggerId: build.GetBuildTriggerId(),
	}
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

	if build.Status == cloudbuildpb.Build_PENDING {
		return g.sendCreateNotification(ctx, build, owner, repo)
	} else {
		return g.sendUpdateNotification(ctx, build, owner, repo)
	}
}

func (g *githubdeploymentsNotifier) sendCreateNotification(ctx context.Context, build *cloudbuildpb.Build, owner string, repo string) error {
	log.Infof("build: %+v", build)
	webhookURL := fmt.Sprintf("%s/%s/%s/deployments", githubApiEndpoint, owner, repo)

	log.Infof("sending GitHub Deployment webhook for Build %q (status: %q) to url %q", build.Id, build.Status, webhookURL)

	bindings, err := g.br.Resolve(ctx, nil, build)
	if err != nil {
		log.Errorf("failed to resolve bindings: %v", err)
	}
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
	if err := g.createTmpl.Execute(&buf, g.tmplView); err != nil {
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

	log.V(2).Infoln("send HTTP request successfully")
	return nil
}

func (g *githubdeploymentsNotifier) sendUpdateNotification(ctx context.Context, build *cloudbuildpb.Build, owner string, repo string) error {
	log.Infof("build: %+v", build)
	webhookURL := fmt.Sprintf("%s/%s/%s/deployments/%s", githubApiEndpoint, owner, repo, "deployment-id-here")
	log.Infof("sending GitHub Deployment webhook for Build %q (status: %q) to url %q", build.Id, build.Status, webhookURL)
	// TODO
	return nil
}
