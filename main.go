package main

import (
	"crypto/tls"
	"distributedScheduling/client"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/golang/glog"
	"k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"log"
	"net/http"
	"os"
	"time"
)

type PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

var Patches []PatchOperation
var (
	scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	// 在 init 函数中定义和设置标志
	flag.Set("logtostderr", "false")
	flag.Set("alsologtostderr", "false")
	flag.Set("log_dir", "/var/log/myapp")
}

func main() {
	fmt.Println("Original flags:", os.Args)

	// 过滤掉 --no-opt 和其他不需要的标志
	var filteredArgs []string
	for _, arg := range os.Args {
		if arg != "--no-opt" && arg != "-r" {
			filteredArgs = append(filteredArgs, arg)
		}
	}

	// 替换 os.Args 并重新解析
	os.Args = filteredArgs
	flag.Parse()

	fmt.Println("Filtered flags:", os.Args)
	go func() {
		for {
			glog.Flush()
			time.Sleep(1 * time.Second) // Log refreshed every 1 second
		}
	}()

	// Read certificate file and private key file
	cert, err := tls.LoadX509KeyPair("/certs/tls.crt", "/certs/tls.key")
	if err != nil {
		glog.Errorf("get cert fail.err is :", err)
		panic(err)
	}

	// Creating a TLS Configuration
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		//ClientCAs:    caCertPool,
		//ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	// Create an HTTP server
	server := &http.Server{
		Addr:      ":8443",
		TLSConfig: tlsConfig,
	}

	// start services
	http.Handle("/webhook", New(&distributedScheduling{}))
	client.NewClientK8s()
	if err := server.ListenAndServeTLS("", ""); err != nil {
		glog.Errorf("server start fail,err is:", err)
		panic(err)
	}
}

type distributedScheduling struct{}

func (ch *distributedScheduling) handler(w http.ResponseWriter, r *http.Request) {
	var writeErr error
	if bytes, err := webHookVerify(w, r); err != nil {
		glog.Errorf("Error handling webhook request: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, writeErr = w.Write([]byte(err.Error()))
	} else {
		log.Print("Webhook request handled successfully")
		_, writeErr = w.Write(bytes)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}
	if writeErr != nil {
		glog.Errorf("Could not write response: %v", writeErr)
	}
	return
}

func webHookVerify(w http.ResponseWriter, r *http.Request) (bytes []byte, err error) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil, fmt.Errorf("invalid method %s, only POST requests are allowed", r.Method)
	}

	if contentType := r.Header.Get("Content-Type"); contentType != `application/json` {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("unsupported content type %s, only %s is supported", contentType, `application/json`)
	}

	var admissionReviewReq v1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&admissionReviewReq); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("r.Body parsing failed: %v", err)
	} else if admissionReviewReq.Request == nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, errors.New("request is nil")
	}
	glog.Infof("The structure information received by http is :", admissionReviewReq)
	//jsonData, err := json.Marshal(admissionReviewReq)
	//fmt.Println(string(jsonData))

	// Handle Deployment and StatefulSet requests
	if admissionReviewReq.Request.Kind.Kind == "Deployment" {
		return handleDeploymentRequest(admissionReviewReq, w)
	} else if admissionReviewReq.Request.Kind.Kind == "StatefulSet" {
		return handleStatefulSetRequest(admissionReviewReq, w)
	}

	return nil, fmt.Errorf("unsupported request kind %s", admissionReviewReq.Request.Kind.Kind)
}

func handleDeploymentRequest(admissionReviewReq v1.AdmissionReview, w http.ResponseWriter) ([]byte, error) {
	deployment := appsv1.Deployment{}
	_, _, err := Codecs.UniversalDecoder().Decode(admissionReviewReq.Request.Object.Raw, nil, &deployment)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("Request type is not Deployment. err is: %v", err)
	}
	if admissionReviewReq.Request.Namespace == metav1.NamespacePublic || admissionReviewReq.Request.Namespace == metav1.NamespaceSystem {
		glog.Infof("ns is a public resource and is prohibited from being modified. ns is :", admissionReviewReq.Request.Namespace)
		return nil, nil
	}
	// Modify the Deployment to add the appropriate affinity rules
	return deploymentPatch(admissionReviewReq, deployment)
}

func handleStatefulSetRequest(admissionReviewReq v1.AdmissionReview, w http.ResponseWriter) ([]byte, error) {
	statefulSet := appsv1.StatefulSet{}
	_, _, err := Codecs.UniversalDecoder().Decode(admissionReviewReq.Request.Object.Raw, nil, &statefulSet)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("Request type is not StatefulSet. err is: %v", err)
	}
	if admissionReviewReq.Request.Namespace == metav1.NamespacePublic || admissionReviewReq.Request.Namespace == metav1.NamespaceSystem {
		glog.Infof("ns is a public resource and is prohibited from being modified. ns is :", admissionReviewReq.Request.Namespace)
		return nil, nil
	}
	// Modify the StatefulSet to add the appropriate affinity rules
	return statefulSetPatch(admissionReviewReq, statefulSet)
}

func deploymentPatch(admissionReviewReq v1.AdmissionReview, deployment appsv1.Deployment) (bytes []byte, err error) {
	// Modify Deployment to add affinity rules based on replicas
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 1 {
		// Single replica: prioritize spot but allow on-demand
		deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
					{
						Weight: 1,
						Preference: corev1.NodeSelectorTerm{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/capacity",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"spot"},
								},
							},
						},
					},
				},
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/capacity",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"spot", "on-demand"},
								},
							},
						},
					},
				},
			},
		}
	} else {
		// Multiple replicas: prioritize spot, fallback to on-demand
		deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
					{
						Weight: 1,
						Preference: corev1.NodeSelectorTerm{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/capacity",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"spot"},
								},
							},
						},
					},
				},
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/capacity",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"spot", "on-demand"},
								},
							},
						},
					},
				},
			},
		}
	}
	patchOps := []PatchOperation{
		getPatchItem("add", "/spec/template/spec/affinity", deployment.Spec.Template.Spec.Affinity),
	}
	patchBytes, err := json.Marshal(patchOps)
	if err != nil {
		return nil, err
	}

	admissionReviewResponse := v1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AdmissionReview",
			APIVersion: "admission.k8s.io/v1",
		},
		Response: &v1.AdmissionResponse{
			UID:     admissionReviewReq.Request.UID,
			Allowed: true,
			Patch:   patchBytes,
			PatchType: func() *v1.PatchType {
				pt := v1.PatchTypeJSONPatch
				return &pt
			}(),
		},
	}
	return json.Marshal(&admissionReviewResponse)
}

func statefulSetPatch(admissionReviewReq v1.AdmissionReview, statefulSet appsv1.StatefulSet) (bytes []byte, err error) {
	// Modify StatefulSet to add affinity rules based on replicas
	if statefulSet.Spec.Replicas != nil && *statefulSet.Spec.Replicas == 1 {
		// Single replica: schedule on on-demand node to ensure stability
		statefulSet.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/capacity",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"on-demand"},
								},
							},
						},
					},
				},
			},
		}
	} else {
		// Multiple replicas: distribute 70% on on-demand, 30% on spot nodes
		statefulSet.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
					{
						Weight: 7,
						Preference: corev1.NodeSelectorTerm{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/capacity",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"on-demand"},
								},
							},
						},
					},
					{
						Weight: 3,
						Preference: corev1.NodeSelectorTerm{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/capacity",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"spot"},
								},
							},
						},
					},
				},
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "node.kubernetes.io/capacity",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"spot", "on-demand"},
								},
							},
						},
					},
				},
			},
		}
	}
	patchOps := []PatchOperation{
		getPatchItem("add", "/spec/template/spec/affinity", statefulSet.Spec.Template.Spec.Affinity),
	}
	patchBytes, err := json.Marshal(patchOps)
	if err != nil {
		return nil, err
	}

	admissionReviewResponse := v1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AdmissionReview",
			APIVersion: "admission.k8s.io/v1",
		},
		Response: &v1.AdmissionResponse{
			UID:     admissionReviewReq.Request.UID,
			Allowed: true,
			Patch:   patchBytes,
			PatchType: func() *v1.PatchType {
				pt := v1.PatchTypeJSONPatch
				return &pt
			}(),
		},
	}
	return json.Marshal(&admissionReviewResponse)
}

func getPatchItem(op string, path string, val interface{}) PatchOperation {
	return PatchOperation{
		Op:    op,
		Path:  path,
		Value: val,
	}
}

type Handler interface {
	handler(w http.ResponseWriter, r *http.Request)
}

type HandleProxy struct {
	handler Handler
}

func New(handler Handler) *HandleProxy {
	return &HandleProxy{
		handler: handler,
	}
}

// The Handle needs to implement ServeHTTP
func (h *HandleProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	h.handler.handler(w, r)
}
