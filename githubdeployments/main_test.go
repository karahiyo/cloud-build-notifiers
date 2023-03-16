package main

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/GoogleCloudPlatform/cloud-build-notifiers/lib/notifiers"
	cbpb "google.golang.org/genproto/googleapis/devtools/cloudbuild/v1"
)

func TestCreateDeploymentsTemplate(t *testing.T) {
	tmpl, err := template.New("create_deployments_template").Parse(deploymentPayload)
	if err != nil {
		t.Fatalf("template.Parse failed: %v", err)
	}
	build := &cbpb.Build{
		ProjectId: "my-project-id",
		Id:        "some-build-id",
		Status:    cbpb.Build_SUCCESS,
		LogUrl:    "https://some-build.example.com/log/url?foo=bar",
		Substitutions: map[string]string{
			"REF_NAME":         "abcde123",
			"_ENVIRONMENT":     "test",
			"_ENVIRONMENT_URL": "https://some-service.example.com",
		},
	}

	view := &notifiers.TemplateView{
		Build: &notifiers.BuildView{
			Build: build,
		},
		Params: map[string]string{"buildStatus": "SUCCESS"},
	}

	body := new(bytes.Buffer)
	if err := tmpl.Execute(body, view); err != nil {
		t.Fatalf("failed to execute template: %v", err)
	}

	if !strings.Contains(body.String(), `SUCCESS`) {
		t.Error("missing status")
	}
}
