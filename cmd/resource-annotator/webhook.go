// Copyright Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/containers/nri-plugins/pkg/kubernetes"
	logger "github.com/containers/nri-plugins/pkg/log"

	admv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/yaml"
)

const (
	contentTypeJSON  = "application/json"
	contentTypeProbe = "application/liveness-probe"
	apiKind          = "AdmissionReview"
	apiVersion       = "admission.k8s.io/v1"
	pathAnnotations  = "/metadata/annotations"
)

var (
	pathAnnotatedResources = pathAnnotations + "/" +
		strings.ReplaceAll(kubernetes.AnnotatedResourcesKey, "/", "~1")

	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

type jsonPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

type Webhook struct {
	logger.Logger
	cfg *Config
	srv *http.Server
}

func NewWebhook(cfg *Config) (*Webhook, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	w := &Webhook{
		Logger: logger.Get("webhook"),
		cfg:    cfg,
		srv: &http.Server{
			Addr:      ":" + strconv.FormatUint(uint64(cfg.Port), 10),
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
			Handler:   http.NewServeMux(),
		},
	}

	w.srv.Handler.(*http.ServeMux).HandleFunc("/", w.handler)

	return w, nil
}

func (w *Webhook) Run() error {
	return w.srv.ListenAndServeTLS("", "")
}

func (w *Webhook) handler(rw http.ResponseWriter, r *http.Request) {
	rpl, err := w.processRequest(r)
	if err != nil {
		w.Errorf("request decode/check failed: %v", err)
		if rpl == nil {
			return
		}
	}

	data, err := json.Marshal(rpl)
	if err != nil {
		w.Errorf("failed to marshal HTTP response: %v", err)
		return
	}

	if _, err := rw.Write(data); err != nil {
		w.Errorf("failed to write HTTP response: %v", err)
	}
}

func (w *Webhook) processRequest(r *http.Request) (*admv1.AdmissionReview, error) {
	var (
		req = &admv1.AdmissionReview{}
		rpl = &admv1.AdmissionReview{}
	)

	switch t := r.Header.Get("Content-Type"); t {
	case contentTypeJSON:
	case contentTypeProbe:
		w.Debug("received liveness probe")
		rpl.Response = probeResponse()
		return rpl, nil
	default:
		w.Errorf("unexpected Content-Type %q, expected %q", t, contentTypeJSON)
		return nil, fmt.Errorf("unexpected Content-Type: %q != %q", t, contentTypeJSON)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	_, _, err = codecs.UniversalDeserializer().Decode(body, nil, req)
	if err != nil {
		rpl.Response = errorResponse("", err)
		return rpl, fmt.Errorf("failed to decode request: %w", err)
	}

	if req.Request == nil {
		w.Warnf("got empty request (treating as liveness probe)")
		rpl.Response = probeResponse()
		return rpl, nil
	}

	w.DebugBlock(" <request> ", "%s", AsYaml(req))

	if req.Request.Resource.Group != "" || req.Request.Resource.Version != "v1" {
		w.Errorf("unexpected resource group/version '%s/%s'",
			req.Request.Resource.Group, req.Request.Resource.Version)
		rpl.Response = ignoreAndAllowResponse(req.Request.UID)
		return rpl, err
	}

	if req.Request.Resource.Resource != "pods" {
		w.Errorf("unexpected resource %s", req.Request.Resource)
		rpl.Response = ignoreAndAllowResponse(req.Request.UID)
		return rpl, err
	}

	rpl.Kind = apiKind
	rpl.APIVersion = apiVersion

	response, err := annotateResources(&req.Request.Object)
	if err != nil {
		w.Errorf("failed to annotate resources: %v", err)
		rpl.Response = ignoreAndAllowResponse(req.Request.UID)
	} else {
		rpl.Response = response
	}

	rpl.Response.UID = req.Request.UID

	return rpl, nil
}

func annotateResources(raw *runtime.RawExtension) (*admv1.AdmissionResponse, error) {
	var (
		pod = &corev1.Pod{}
		ops = []jsonPatch{}
	)

	if _, _, err := codecs.UniversalDeserializer().Decode(raw.Raw, nil, pod); err != nil {
		return nil, fmt.Errorf("failed to deserialize pod: %w", err)
	}

	if pod.Annotations == nil {
		ops = append(
			ops,
			jsonPatch{
				Op:    "add",
				Path:  pathAnnotations,
				Value: map[string]string{},
			},
		)
	}

	annotated := kubernetes.AnnotatedResources{}

	if len(pod.Spec.Containers) > 0 {
		annotated.Containers = map[string]corev1.ResourceRequirements{}
		for _, c := range pod.Spec.Containers {
			annotated.Containers[c.Name] = c.Resources
		}
	}

	if len(pod.Spec.InitContainers) > 0 {
		annotated.InitContainers = map[string]corev1.ResourceRequirements{}
		for _, c := range pod.Spec.InitContainers {
			annotated.InitContainers[c.Name] = c.Resources
		}
	}

	data, err := annotated.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal annotated resources: %w", err)
	}

	ops = append(
		ops,
		jsonPatch{
			Op:    "add",
			Path:  pathAnnotatedResources,
			Value: string(data),
		},
	)

	data, err = json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pod patches: %w", err)
	}

	patchType := admv1.PatchTypeJSONPatch
	return &admv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     data,
	}, nil
}

func probeResponse() *admv1.AdmissionResponse {
	return &admv1.AdmissionResponse{
		Result: &metav1.Status{
			Message: "liveness probe response",
			Status:  metav1.StatusSuccess,
		},
	}
}

func ignoreAndAllowResponse(uid types.UID) *admv1.AdmissionResponse {
	return &admv1.AdmissionResponse{
		UID:     uid,
		Allowed: true,
	}
}

func errorResponse(uid types.UID, err error) *admv1.AdmissionResponse {
	return &admv1.AdmissionResponse{
		UID: uid,
		Result: &metav1.Status{
			Message: err.Error(),
		},
	}
}

func AsYaml(obj interface{}) string {
	data, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Sprintf("%%!(MARSHAL-FAILED type=%T, err=%v)", obj, err)
	}
	return string(data)
}

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(admv1.AddToScheme(scheme))
}
