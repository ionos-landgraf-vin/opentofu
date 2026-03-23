package tfplugin6

import "os"

func init() {
	// This package and github.com/hashicorp/terraform-plugin-go/tfprotov6/internal/tfplugin6
	// both register "tfplugin6.proto" into the global protobuf registry.  The schemas are
	// identical; downgrade the panic to a warning so binaries that embed OpenTofu alongside
	// an in-process Terraform provider start cleanly.
	//
	// Named conflict_init.go ('c' < 't') so this init() always runs before tfplugin6.pb.go.
	if os.Getenv("GOLANG_PROTOBUF_REGISTRATION_CONFLICT") == "" {
		_ = os.Setenv("GOLANG_PROTOBUF_REGISTRATION_CONFLICT", "warn")
	}
}
