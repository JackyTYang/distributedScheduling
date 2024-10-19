package main

import (
	"context"
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
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
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

// 定义用于存储Pod分布情况的结构体
type PodDistribution struct {
	Spot     int
	OnDemand int
}

// 定义嵌套映射：namespace -> appLabel -> PodDistribution
var podDistribution = make(map[string]map[string]*PodDistribution)

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
	// 将 pod 加入 informer，并注册 event handler 函数
	// 该方法不可行。因为pod会批量调度，webhook处理下一个pod前，上一个pod可能还未被监听到，导致一大批pod都用的是最初的分布
	//informer := watchPods()
	//stopCh := make(chan struct{})
	//go informer.Run(stopCh)
}

func watchPods() cache.SharedInformer {
	// 创建一个不带命名空间和标签过滤的 SharedInformerFactory
	factory := informers.NewSharedInformerFactory(client.K8sClient.Api, 0)

	// 创建 Pod 的 Informer
	podInformer := factory.Core().V1().Pods().Informer()

	// 添加事件处理函数
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			glog.Infof("Pod added: %s/%s", pod.Namespace, pod.Name)

			// 检查标签是否为 ds-admission-webhook/autoSchedule: "enabled"
			if value, ok := pod.Labels["ds-admission-webhook/autoSchedule"]; !ok || value != "enabled" {
				glog.Infof("Pod %s/%s does not have the required label or it's not enabled, skipping.", pod.Namespace, pod.Name)
				return
			}

			// 获取 Pod 所在的节点名称
			nodeName := pod.Spec.NodeName
			if nodeName == "" {
				glog.Infof("Pod %s/%s is not yet scheduled to a node, skipping.", pod.Namespace, pod.Name)
				return
			}

			// 获取节点信息
			node, err := client.K8sClient.Api.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				glog.Errorf("Failed to get node %s for pod %s/%s: %v", nodeName, pod.Namespace, pod.Name, err)
				return
			}

			// 确定节点的容量类型（spot 或 on-demand）
			capacityType, ok := node.Labels["node.kubernetes.io/capacity"]
			if !ok {
				glog.Infof("Node %s does not have the 'node.kubernetes.io/capacity' label, skipping.", nodeName)
				return
			}

			// 更新 PodDistribution 数据
			// 检查并初始化嵌套的映射
			if _, ok := podDistribution[pod.Namespace]; !ok {
				podDistribution[pod.Namespace] = make(map[string]*PodDistribution)
			}
			if _, ok := podDistribution[pod.Namespace]["app"]; !ok {
				podDistribution[pod.Namespace]["app"] = &PodDistribution{}
			}
			if capacityType == "spot" {
				podDistribution[pod.Namespace]["app"].Spot++
			} else if capacityType == "on-demand" {
				podDistribution[pod.Namespace]["app"].OnDemand++
			}
			glog.Infof("Updated pod distribution for %s/%s on node %s (%s)", pod.Namespace, pod.Name, nodeName, capacityType)
		},
	})
	return podInformer
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
	} else if admissionReviewReq.Request.Kind.Kind == "Pod" {
		return handlePodRequest(admissionReviewReq, w)
	}

	return nil, fmt.Errorf("unsupported request kind %s", admissionReviewReq.Request.Kind.Kind)
}

func handlePodRequest(admissionReviewReq v1.AdmissionReview, w http.ResponseWriter) ([]byte, error) {
	pod := corev1.Pod{}
	_, _, err := Codecs.UniversalDecoder().Decode(admissionReviewReq.Request.Object.Raw, nil, &pod)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("Request type is not Deployment. err is: %v", err)
	}
	if admissionReviewReq.Request.Namespace == metav1.NamespacePublic || admissionReviewReq.Request.Namespace == metav1.NamespaceSystem {
		glog.Infof("ns is a public resource and is prohibited from being modified. ns is :", admissionReviewReq.Request.Namespace)
		return nil, nil
	}

	// modify the pod according to the current pod scheduling distribution
	namespace := pod.Namespace
	appLabel := pod.ObjectMeta.Labels["app"]

	// 检查并初始化嵌套的映射
	if _, ok := podDistribution[namespace]; !ok {
		podDistribution[namespace] = make(map[string]*PodDistribution)
	}
	if _, ok := podDistribution[namespace][appLabel]; !ok {
		podDistribution[namespace][appLabel] = &PodDistribution{}
	}

	// 检查该类deploy下的pod是否有调度分布数据
	//if podDistribution[namespace][appLabel].OnDemand == 0 && podDistribution[namespace][appLabel].Spot == 0 {
	//	// 使用clientSet list机制构建数据
	//	err := buildPodDistributionData(namespace, appLabel)
	//	if err != nil {
	//		glog.Errorf("Failed to build pod distribution data: %v", err)
	//		return nil, err
	//	}
	//}

	// 每次都重新计算
	err = buildPodDistributionData(namespace, appLabel)
	if err != nil {
		glog.Errorf("Failed to build pod distribution data: %v", err)
		return nil, err
	}

	var actualSpotWeightRatio, anticipatedSpotWeightRatio float64
	var target string
	// 计算预期比值
	if 100-getPodSpotWeight(&pod) == 0 {
		anticipatedSpotWeightRatio = math.MaxFloat64
	} else {
		anticipatedSpotWeightRatio = float64(getPodSpotWeight(&pod)) / float64(100-getPodSpotWeight(&pod))
	}

	// 计算实际比值
	if podDistribution[namespace][appLabel].Spot == 0 && podDistribution[namespace][appLabel].OnDemand == 0 {
		return podPatch(admissionReviewReq, "nothing") // 是第一个节点，直接按照原有affinity策略进行
	} else if podDistribution[namespace][appLabel].OnDemand == 0 {
		actualSpotWeightRatio = math.MaxFloat64
	} else {
		actualSpotWeightRatio = float64(podDistribution[namespace][appLabel].Spot) / float64(podDistribution[namespace][appLabel].OnDemand)
	}

	// 根据两者大小判断pod调度节点
	if actualSpotWeightRatio > anticipatedSpotWeightRatio {
		target = "on-demand"
	} else if actualSpotWeightRatio < anticipatedSpotWeightRatio {
		target = "spot"
	} else {
		target = "nothing"
	}

	return podPatch(admissionReviewReq, target)
}

// 使用clientSet构建Pod分布数据
func buildPodDistributionData(namespace, appLabel string) error {

	//初始化数据
	podDistribution[namespace][appLabel].OnDemand = 0
	podDistribution[namespace][appLabel].Spot = 0

	// 使用K8sClient.Api获取当前namespace下，符合app标签的Pod列表
	podList, err := client.K8sClient.Api.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", appLabel),
	})
	if err != nil {
		return fmt.Errorf("failed to list pods in namespace %s with label app=%s: %v", namespace, appLabel, err)
	}

	// 遍历Pod列表，根据Pod所在节点的node.kubernetes.io/capacity标签构建分布
	for _, pod := range podList.Items {
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			// Pod 还未调度到节点上，跳过
			continue
		}

		// 获取节点信息
		node, err := client.K8sClient.Api.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			glog.Errorf("Failed to get node %s: %v", nodeName, err)
			continue
		}

		// 判断节点的 node.kubernetes.io/capacity 标签
		capacityType, ok := node.Labels["node.kubernetes.io/capacity"]
		if !ok {
			glog.Infof("Node %s does not have the node.kubernetes.io/capacity label", nodeName)
			continue
		}

		// 更新分布数据
		if capacityType == "spot" {
			podDistribution[namespace][appLabel].Spot++
		} else if capacityType == "on-demand" {
			podDistribution[namespace][appLabel].OnDemand++
		}
	}

	return nil
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

func podPatch(admissionReviewReq v1.AdmissionReview, target string) ([]byte, error) {
	// 如果 target 是 "nothing"，直接返回允许的 AdmissionReview 响应
	if target == "nothing" {
		admissionReviewResponse := v1.AdmissionReview{
			TypeMeta: metav1.TypeMeta{
				Kind:       "AdmissionReview",
				APIVersion: "admission.k8s.io/v1",
			},
			Response: &v1.AdmissionResponse{
				UID:     admissionReviewReq.Request.UID,
				Allowed: true, // 允许请求通过
			},
		}
		// 序列化并返回 AdmissionReview 响应
		return json.Marshal(&admissionReviewResponse)
	}

	// 根据 target 参数设置 nodeSelector 的值
	var nodeSelectorValues []string
	switch target {
	case "spot":
		nodeSelectorValues = []string{"spot"}
	case "on-demand":
		nodeSelectorValues = []string{"on-demand"}
	default:
		// 如果传入的 target 值无效，返回错误
		return nil, fmt.Errorf("invalid target value: %s, must be 'spot' or 'on-demand'", target)
	}

	// 构建 NodeAffinity 的 RequiredDuringSchedulingIgnoredDuringExecution
	nodeAffinity := &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      "node.kubernetes.io/capacity",
							Operator: corev1.NodeSelectorOpIn,
							Values:   nodeSelectorValues,
						},
					},
				},
			},
		},
	}

	// 创建 Patch 操作，将 NodeAffinity 添加到 Pod 的 spec 中
	patchOps := []PatchOperation{
		getPatchItem("replace", "/spec/affinity/nodeAffinity", nodeAffinity),
	}

	// 序列化 patch 操作为 JSON 字节数组
	patchBytes, err := json.Marshal(patchOps)
	if err != nil {
		return nil, err
	}

	// 构建 AdmissionReview 响应
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

	// 返回序列化后的 AdmissionReview 响应
	return json.Marshal(&admissionReviewResponse)
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
		weight := getDeploymentSpotWeight(&deployment)
		appName := deployment.ObjectMeta.Labels["app"]
		// Multiple replicas: prioritize spot, fallback to on-demand
		deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
					{
						Weight: weight,
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
					{
						Weight: 100 - weight,
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
			// 配置 PodAntiAffinity
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
					{
						Weight: 1,
						PodAffinityTerm: corev1.PodAffinityTerm{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": appName, // 用实际的 app 名称替换 stateful-app
								},
							},
							TopologyKey: "kubernetes.io/hostname", // 确保不同副本分布在不同节点上
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
		weight := getStatefulSetSpotWeight(&statefulSet)
		appName := statefulSet.ObjectMeta.Labels["app"]
		// Multiple replicas: distribute 70% on on-demand, 30% on spot nodes
		statefulSet.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
					{
						Weight: 100 - weight,
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
						Weight: weight,
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
			// 配置 PodAntiAffinity
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
					{
						Weight: 1,
						PodAffinityTerm: corev1.PodAffinityTerm{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": appName, // 用实际的 app 名称替换 stateful-app
								},
							},
							TopologyKey: "kubernetes.io/hostname", // 确保不同副本分布在不同节点上
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

func getPodSpotWeight(pod *corev1.Pod) int32 {
	spotWeight, exists := pod.ObjectMeta.Labels["ds-admission-webhook/spotWeight"]
	if !exists || spotWeight == "" {
		spotWeight = "100" // default weight, all on spot by default
	}
	weightInt, err := strconv.Atoi(spotWeight)
	if err != nil {
		// 如果转换出错，使用默认值
		weightInt = 100
	}

	// 将weightInt从int类型转换为int32
	weight := int32(weightInt)
	return weight
}

func getDeploymentSpotWeight(deployment *appsv1.Deployment) int32 {
	spotWeight, exists := deployment.ObjectMeta.Labels["ds-admission-webhook/spotWeight"]
	if !exists || spotWeight == "" {
		spotWeight = "100" // default weight, all on spot by default
	}
	weightInt, err := strconv.Atoi(spotWeight)
	if err != nil {
		// 如果转换出错，使用默认值
		weightInt = 100
	}

	// 将weightInt从int类型转换为int32
	weight := int32(weightInt)
	return weight
}

func getStatefulSetSpotWeight(statefulset *appsv1.StatefulSet) int32 {
	spotWeight, exists := statefulset.ObjectMeta.Labels["ds-admission-webhook/spotWeight"]
	if !exists || spotWeight == "" {
		spotWeight = "30" // default weight
	}
	weightInt, err := strconv.Atoi(spotWeight)
	if err != nil {
		// 如果转换出错，使用默认值
		weightInt = 30
	}

	// 将weightInt从int类型转换为int32
	weight := int32(weightInt)
	return weight
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
