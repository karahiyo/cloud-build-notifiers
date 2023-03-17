package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	cbpb "google.golang.org/genproto/googleapis/devtools/cloudbuild/v1"
)

func TestCreateDeploymentsPayload(t *testing.T) {
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
	msg := createDeploymentMessage{
		Environment: build.Substitutions["_ENVIRONMENT"],
		Ref:         build.Substitutions["REF_NAME"],
		Description: fmt.Sprintf("Cloud Build (%s) %s status: %s, trigger_id: %s", build.ProjectId, build.Id, build.Status, build.BuildTriggerId),
		Payload:     "",
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to json marshal: %v", err)
	}

	if !strings.Contains(string(payload), `Cloud Build`) {
		t.Error("missing status")
	}
}
