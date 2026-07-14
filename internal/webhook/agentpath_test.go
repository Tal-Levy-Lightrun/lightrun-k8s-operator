package webhook

import (
	"strings"
	"testing"
)

// Test_buildAgentPathArg is the webhook-package equivalent of
// internal/controller/helpers_test.go's Test_agentEnvVarArgument. It pins down the same
// "-agentpath:<mountPath>/agent/lightrun_agent.so[=<cliFlags>]" format and the same
// hard 1024-char ceiling (a Java limitation, not ours to relax) that
// internal/controller/helpers.go's agentEnvVarArgument enforces today. The webhook must
// carry this behavior over unchanged: buildAgentPathArg(mountPath, agentCliFlags string)
// (string, error) does not exist yet -- this is part of the RED signal for this package.
func Test_buildAgentPathArg(t *testing.T) {
	type args struct {
		mountPath     string
		agentCliFlags string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name: "mount path and cli flags produce agentpath with cli flags appended",
			args: args{
				mountPath:     "/lightrun",
				agentCliFlags: "--lightrun_extra_class_path=<PATH_TO_JAR>",
			},
			want:    "-agentpath:/lightrun/agent/lightrun_agent.so=--lightrun_extra_class_path=<PATH_TO_JAR>",
			wantErr: false,
		},
		{
			name: "no cli flags produce bare agentpath",
			args: args{
				mountPath:     "/lightrun",
				agentCliFlags: "",
			},
			want:    "-agentpath:/lightrun/agent/lightrun_agent.so",
			wantErr: false,
		},
		{
			name: "agentpath with cli flags over 1024 chars is rejected (Java limitation)",
			args: args{
				mountPath:     "/lightrun",
				agentCliFlags: strings.Repeat("a", 1024),
			},
			want:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildAgentPathArg(tt.args.mountPath, tt.args.agentCliFlags)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildAgentPathArg() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("buildAgentPathArg() = %v, want %v", got, tt.want)
			}
		})
	}
}
