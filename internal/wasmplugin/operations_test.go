package wasmplugin

import (
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestValidateOperationsDefaultsTargetsAndEnforcesCapabilities(t *testing.T) {
	source := domain.WASMPluginObject{Bucket: "images", Key: "incoming/a.jpg"}
	plugin := domain.WASMPlugin{OperationPolicy: domain.WASMPluginOperationPolicy{
		AllowedOperations: []string{domain.WASMPluginOperationMetadataPatch, domain.WASMPluginOperationObjectCopy},
		BucketPatterns:    []string{"archive-*"},
		KeyPrefixes:       []string{"incoming/", "processed/"},
		MaxOperations:     2,
	}}
	operations := []domain.WASMPluginOperation{
		{Type: domain.WASMPluginOperationMetadataPatch, Metadata: map[string]string{"face-count": "2"}},
		{ID: "archive", Type: domain.WASMPluginOperationObjectCopy, Bucket: "archive-eu", Key: "processed/a.jpg"},
	}
	if err := ValidateOperations(plugin, source, operations); err != nil {
		t.Fatal(err)
	}
	if operations[0].ID != "operation-1" || operations[0].Bucket != "images" || operations[0].Key != source.Key {
		t.Fatalf("defaulted operation = %+v", operations[0])
	}
	if operations[1].SourceBucket != "images" || operations[1].SourceKey != source.Key {
		t.Fatalf("defaulted copy source = %+v", operations[1])
	}
}

func TestValidateOperationsRejectsDeniedUnsafeAndOutOfScopeCommands(t *testing.T) {
	source := domain.WASMPluginObject{Bucket: "images", Key: "incoming/a.jpg"}
	tests := []struct {
		name   string
		policy domain.WASMPluginOperationPolicy
		op     domain.WASMPluginOperation
		want   string
	}{
		{name: "deny by default", op: domain.WASMPluginOperation{Type: domain.WASMPluginOperationObjectDelete}, want: "not allowed"},
		{name: "unsafe key", policy: domain.WASMPluginOperationPolicy{AllowedOperations: []string{domain.WASMPluginOperationObjectCopy}}, op: domain.WASMPluginOperation{Type: domain.WASMPluginOperationObjectCopy, Key: "../secret"}, want: "unsafe"},
		{name: "cross bucket", policy: domain.WASMPluginOperationPolicy{AllowedOperations: []string{domain.WASMPluginOperationObjectCopy}}, op: domain.WASMPluginOperation{Type: domain.WASMPluginOperationObjectCopy, Bucket: "archive", Key: "copy.jpg"}, want: "outside"},
		{name: "key prefix", policy: domain.WASMPluginOperationPolicy{AllowedOperations: []string{domain.WASMPluginOperationObjectDelete}, KeyPrefixes: []string{"safe/"}}, op: domain.WASMPluginOperation{Type: domain.WASMPluginOperationObjectDelete, Key: "other/file"}, want: "outside"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateOperations(domain.WASMPlugin{OperationPolicy: test.policy}, source, []domain.WASMPluginOperation{test.op})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}
