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

import "testing"

// Test_parseUseMountedFiles pins down parseUseMountedFiles' handling of the
// lightrun.com/use-secrets-as-mounted-files annotation value: absent or unparseable
// defaults to true (matching the old CRD field's kubebuilder default, see section 8's
// "resolved pool secretRef wiring" specs in pod_mutator_test.go), while "true"/"false"
// parse via strconv.ParseBool.
func Test_parseUseMountedFiles(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "absent defaults to true", raw: "", want: true},
		{name: "explicit true", raw: "true", want: true},
		{name: "explicit false", raw: "false", want: false},
		{name: "unparseable value defaults to true", raw: "not-a-bool", want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseUseMountedFiles(c.raw); got != c.want {
				t.Errorf("parseUseMountedFiles(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}
