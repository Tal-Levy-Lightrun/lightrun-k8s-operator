/*
Copyright 2022 Lightrun

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import "errors"

// buildAgentPathArg mirrors internal/controller/helpers.go's agentEnvVarArgument: it builds
// the "-agentpath:<mountPath>/agent/lightrun_agent.so[=<agentCliFlags>]" argument and enforces
// the same hard 1024-char ceiling (a Java limitation, not ours to relax).
func buildAgentPathArg(mountPath string, agentCliFlags string) (string, error) {
	agentArg := "-agentpath:" + mountPath + "/agent/lightrun_agent.so"
	if agentCliFlags != "" {
		agentArg += "=" + agentCliFlags
		if len(agentArg) > 1024 {
			return "", errors.New("agentpath with agentCliFlags has more than 1024 chars. This is a limitation of Java")
		}
	}
	return agentArg, nil
}
