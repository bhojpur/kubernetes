/*
Copyright 2017 The Kubernetes Authors.

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

package aggregator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"k8s.io/kube-openapi/pkg/handler3"
)

type handlerTest struct {
	etag string
	data []byte
}

var _ http.Handler = handlerTest{}

func (h handlerTest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Create an APIService with a handler for one group/version
	if r.URL.Path == "/openapi/v3" {
		group := &handler3.OpenAPIV3Discovery{
			Paths: map[string]handler3.OpenAPIV3DiscoveryGroupVersion{
				"apis/group/version": {
					ServerRelativeURL: "/openapi/v3/apis/group/version?hash=" + h.etag,
				},
			},
		}

		j, _ := json.Marshal(group)
		w.Write(j)
		return
	}

	if r.URL.Path == "/openapi/v3/apis/group/version" {
		if len(h.etag) > 0 {
			w.Header().Add("Etag", h.etag)
		}
		ifNoneMatches := r.Header["If-None-Match"]
		for _, match := range ifNoneMatches {
			if match == h.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Write(h.data)
	}
}

func assertDownloadedSpec(gvSpec map[string]*SpecETag, err error, expectedSpecID string, expectedEtag string) error {
	if err != nil {
		return fmt.Errorf("downloadOpenAPISpec failed : %s", err)
	}
	specInfo, ok := gvSpec["apis/group/version"]
	if !ok {
		if expectedSpecID == "" {
			return nil
		}
		return fmt.Errorf("expected to download spec, no spec downloaded")
	}

	if specInfo.spec != nil && expectedSpecID == "" {
		return fmt.Errorf("expected ID %s, actual ID %s", expectedSpecID, specInfo.spec.Version)
	}

	if specInfo.spec != nil && specInfo.spec.Version != expectedSpecID {
		return fmt.Errorf("expected ID %s, actual ID %s", expectedSpecID, specInfo.spec.Version)
	}
	if specInfo.etag != expectedEtag {
		return fmt.Errorf("expected ETag '%s', actual ETag '%s'", expectedEtag, specInfo.etag)
	}
	return nil
}

func TestDownloadOpenAPISpec(t *testing.T) {
	s := Downloader{}

	groups, err := s.OpenAPIV3Root(
		handlerTest{data: []byte(""), etag: ""})
	assert.NoError(t, err)
	if assert.NotNil(t, groups) {
		assert.Equal(t, len(groups.Paths), 1)
		if assert.Contains(t, groups.Paths, "apis/group/version") {
			assert.NotEmpty(t, groups.Paths["apis/group/version"].ServerRelativeURL)
		}
	}

	// Test with eTag
	gvSpec, err := s.Download(
		handlerTest{data: []byte("{\"openapi\": \"test\"}"), etag: "etag_test"}, map[string]string{})
	assert.NoError(t, assertDownloadedSpec(gvSpec, err, "test", "etag_test"))

	// Test not modified
	gvSpec, err = s.Download(
		handlerTest{data: []byte("{\"openapi\": \"test\"}"), etag: "etag_test"}, map[string]string{"apis/group/version": "etag_test"})
	assert.NoError(t, assertDownloadedSpec(gvSpec, err, "", "etag_test"))

	// Test different eTags
	gvSpec, err = s.Download(
		handlerTest{data: []byte("{\"openapi\": \"test\"}"), etag: "etag_test1"}, map[string]string{"apis/group/version": "etag_test2"})
	assert.NoError(t, assertDownloadedSpec(gvSpec, err, "test", "etag_test1"))
}
