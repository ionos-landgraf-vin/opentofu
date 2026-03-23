package tfplugin5

import "os"

func init() {
	// This package and github.com/hashicorp/terraform-plugin-go/tfprotov5/internal/tfplugin5
	// both register "tfplugin5.proto" into the global protobuf registry.  The schemas are
	// identical; downgrade the panic to a warning so binaries that embed OpenTofu alongside
	// an in-process Terraform provider start cleanly.
	//
	// Named conflict_init.go ('c' < 't') so this init() always runs before tfplugin5.pb.go.
	if os.Getenv("GOLANG_PROTOBUF_REGISTRATION_CONFLICT") == "" {
		_ = os.Setenv("GOLANG_PROTOBUF_REGISTRATION_CONFLICT", "warn")
	}
}
