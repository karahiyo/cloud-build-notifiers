package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/cloud-build-notifiers/lib/notifiers"
	log "github.com/golang/glog"
	cbpb "google.golang.org/genproto/googleapis/devtools/cloudbuild/v1"
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
	tmpl        *template.Template
	githubToken string
	githubRepo  string

	br       notifiers.BindingResolver
	tmplView *notifiers.TemplateView
}

type githubdeploymentsMessage struct {
	Env    string `json:"env"`
	EnvUrl string `json:"env_url"`
	LogUrl string `json:"log_url"`
}

func (g *githubdeploymentsNotifier) SetUp(ctx context.Context, cfg *notifiers.Config, deploymentsTemplate string, sg notifiers.SecretGetter, br notifiers.BindingResolver) error {
	prd, err := notifiers.MakeCELPredicate(cfg.Spec.Notification.Filter)
	if err != nil {
		return fmt.Errorf("failed to make a CEL predicate: %w", err)
	}
	g.filter = prd
	g.br = br

	repo, ok := cfg.Spec.Notification.Delivery["githubRepo"].(string)
	if !ok {
		return fmt.Errorf("expected delivery config %v to have string field `githubRepo`", cfg.Spec.Notification.Delivery)
	}
	g.githubRepo = repo

	tmpl, err := template.New("deployments_template").Parse(deploymentsTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse deployment body template: %w", err)
	}
	g.tmpl = tmpl

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

func (g *githubdeploymentsNotifier) SendNotification(ctx context.Context, build *cbpb.Build) error {
	if !g.filter.Apply(ctx, build) {
		log.V(2).Infof("not sending response for event (build id = %s, status = %v)", build.Id, build.Status)
		return nil
	}

	webhookURL := fmt.Sprintf("%s/%s/deployments", githubApiEndpoint, g.githubRepo)

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
	if err := g.tmpl.Execute(&buf, g.tmplView); err != nil {
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
