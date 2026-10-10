package substrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"
)

// EnsureTemplate makes the atespace and the ActorTemplate in file (YAML, the
// protojson shape `kubectl ate create actor-template -f` takes) if they are
// missing, and waits for the template's golden snapshot. The template is
// named <base>-<hash of the file>, so a changed file (a new guest image) is a
// new template; actors made from the old one keep it. It returns that name,
// which is what Options.Template should be.
//
// This is what lets a cluster deploy sandpit declaratively: the file is a
// ConfigMap, and nothing but sandpit talks to ate-api.
func EnsureTemplate(ctx context.Context, api ateapipb.ControlClient, atespace, base, file string) (string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("template file: %w", err)
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return "", fmt.Errorf("template file %s: %w", file, err)
	}
	t := &ateapipb.ActorTemplate{}
	if err := protojson.Unmarshal(js, t); err != nil {
		return "", fmt.Errorf("template file %s: %w", file, err)
	}
	sum := sha256.Sum256(raw)
	name := base + "-" + hex.EncodeToString(sum[:])[:10]
	if t.Metadata == nil {
		t.Metadata = &ateapipb.ResourceMetadata{}
	}
	t.Metadata.Atespace, t.Metadata.Name = atespace, name

	if _, err := api.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: atespace}},
	}); err != nil && status.Code(err) != codes.AlreadyExists {
		return "", fmt.Errorf("atespace %s: %w", atespace, err)
	}
	ref := &ateapipb.ObjectRef{Atespace: atespace, Name: name}
	if _, err := api.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: ref}); status.Code(err) == codes.NotFound {
		if _, err := api.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{ActorTemplate: t}); err != nil && status.Code(err) != codes.AlreadyExists {
			return "", fmt.Errorf("template %s/%s: %w", atespace, name, err)
		}
	} else if err != nil {
		return "", fmt.Errorf("template %s/%s: %w", atespace, name, err)
	}

	// Actors can't resume until the golden snapshot exists.
	for {
		got, err := api.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: ref})
		if err != nil {
			return "", fmt.Errorf("template %s/%s: %w", atespace, name, err)
		}
		if got.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag() != nil {
			return name, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("template %s/%s: no golden snapshot yet: %w", atespace, name, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
