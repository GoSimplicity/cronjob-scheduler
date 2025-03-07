/*
 * Copyright 2024 Bamboo
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * File: main.go
 * Description: Kubernetes CronJob 动态调度器
 */

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/retry"
)

// JobConfig 定义单个Job的配置
type JobConfig struct {
	Name         string              `yaml:"name"`
	Schedule     string              `yaml:"schedule"`
	Image        string              `yaml:"image"`
	Command      []string            `yaml:"command"`
	Args         []string            `yaml:"args,omitempty"`
	Env          []EnvVar            `yaml:"env,omitempty"`
	Resources    ResourceConfig      `yaml:"resources,omitempty"`
	Volumes      []VolumeConfig      `yaml:"volumes,omitempty"`
	VolumeMounts []VolumeMountConfig `yaml:"volumeMounts,omitempty"`
	Labels       map[string]string   `yaml:"labels,omitempty"`
	Annotations  map[string]string   `yaml:"annotations,omitempty"`
}

// EnvVar 定义环境变量
type EnvVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// ResourceConfig 定义资源配置
type ResourceConfig struct {
	Requests ResourceValues `yaml:"requests,omitempty"`
	Limits   ResourceValues `yaml:"limits,omitempty"`
}

// ResourceValues 定义资源值
type ResourceValues struct {
	Memory string `yaml:"memory,omitempty"`
	CPU    string `yaml:"cpu,omitempty"`
}

// VolumeConfig 定义卷配置
type VolumeConfig struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"` // configMap, secret, emptyDir, etc.
	ConfigMap string `yaml:"configMap,omitempty"`
	Secret    string `yaml:"secret,omitempty"`
}

// VolumeMountConfig 定义卷挂载配置
type VolumeMountConfig struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
}

// JobsConfig 包含所有任务配置
type JobsConfig struct {
	Jobs []JobConfig `yaml:"jobs"`
}

// Scheduler 调度器结构体
type Scheduler struct {
	client         *kubernetes.Clientset
	namespace      string
	configMapName  string
	managedJobs    map[string]JobConfig
	stopCh         chan struct{}
	informerStopCh chan struct{}
}

func main() {
	log.Println("启动 CronJob 动态调度器...")

	namespace := getEnv("NAMESPACE", "default")
	configMapName := getEnv("CONFIGMAP_NAME", "cronjob-configs")
	pollInterval := getEnvAsInt("POLL_INTERVAL", 30)
	kubeconfigPath := getEnv("KUBECONFIG", "")

	client, err := getKubernetesClient(kubeconfigPath)
	if err != nil {
		log.Fatalf("获取 Kubernetes 客户端失败: %v", err)
	}

	scheduler := &Scheduler{
		client:         client,
		namespace:      namespace,
		configMapName:  configMapName,
		managedJobs:    make(map[string]JobConfig),
		stopCh:         make(chan struct{}),
		informerStopCh: make(chan struct{}),
	}

	// 设置优雅停机
	setupGracefulShutdown(scheduler)

	// 启动 ConfigMap 监听器
	go scheduler.watchConfigMap()

	// 同步初始状态
	scheduler.syncJobs()

	// 定期检查同步任务
	ticker := time.NewTicker(time.Duration(pollInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			scheduler.syncJobs()
		case <-scheduler.stopCh:
			log.Println("停止调度器...")
			return
		}
	}
}

// 监听 ConfigMap 变化
func (s *Scheduler) watchConfigMap() {
	log.Printf("开始监听 ConfigMap %s/%s 的变化", s.namespace, s.configMapName)

	// 创建监听器
	listWatch := cache.NewListWatchFromClient(
		s.client.CoreV1().RESTClient(),
		"configmaps",
		s.namespace,
		fields.OneTermEqualSelector("metadata.name", s.configMapName), // 使用精确匹配
	)

	_, controller := cache.NewInformer(
		listWatch,
		&corev1.ConfigMap{},
		0, // 不进行重同步
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				s.syncJobs()
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldCM := oldObj.(*corev1.ConfigMap)
				newCM := newObj.(*corev1.ConfigMap)

				// 只有当内容发生变化时才进行同步
				if oldCM.Data["jobs.yaml"] != newCM.Data["jobs.yaml"] {
					log.Println("检测到 ConfigMap 变化，开始同步 CronJob...")
					s.syncJobs()
				}
			},
			DeleteFunc: func(obj interface{}) {
				log.Println("ConfigMap 已删除，清理所有管理的 CronJob...")
				s.deleteAllJobs()
			},
		},
	)

	controller.Run(s.informerStopCh)
}

// 同步任务
func (s *Scheduler) syncJobs() {
	log.Println("开始同步 CronJob...")

	// 获取 ConfigMap
	configMap, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(context.TODO(), s.configMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			log.Printf("ConfigMap %s 不存在，清理所有管理的 CronJob", s.configMapName)
			s.deleteAllJobs()
			return
		}
		log.Printf("获取 ConfigMap 失败: %v", err)
		return
	}

	// 解析配置
	jobsData, exists := configMap.Data["jobs.yaml"]
	if !exists {
		log.Printf("ConfigMap 中没有 jobs.yaml 键，清理所有管理的 CronJob")
		s.deleteAllJobs()
		return
	}

	var config JobsConfig
	err = yaml.Unmarshal([]byte(jobsData), &config)
	if err != nil {
		log.Printf("解析配置失败: %v", err)
		return
	}

	// 跟踪当前配置中的所有任务
	currentJobs := make(map[string]JobConfig)
	for _, jobConfig := range config.Jobs {
		currentJobs[jobConfig.Name] = jobConfig
	}

	// 创建或更新 CronJob
	for _, jobConfig := range currentJobs {
		s.createOrUpdateCronJob(jobConfig)
	}

	// 删除不再需要的 CronJob
	for name := range s.managedJobs {
		if _, exists := currentJobs[name]; !exists {
			s.deleteCronJob(name)
		}
	}
}

// 创建或更新 CronJob
func (s *Scheduler) createOrUpdateCronJob(config JobConfig) {
	log.Printf("处理 CronJob: %s", config.Name)

	// 构建 CronJob 对象
	cronJob := s.buildCronJobObject(config)

	// 尝试获取现有的 CronJob
	existing, err := s.client.BatchV1().CronJobs(s.namespace).Get(context.TODO(), config.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// CronJob 不存在，创建它
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				_, err := s.client.BatchV1().CronJobs(s.namespace).Create(context.TODO(), cronJob, metav1.CreateOptions{})
				return err
			})

			if err != nil {
				log.Printf("创建 CronJob %s 失败: %v", config.Name, err)
				return
			}

			log.Printf("成功创建 CronJob: %s", config.Name)
			s.managedJobs[config.Name] = config
			return
		}

		log.Printf("获取 CronJob %s 失败: %v", config.Name, err)
		return
	}

	// 检查是否需要更新
	if !s.needsUpdate(existing, cronJob) {
		log.Printf("CronJob %s 无需更新", config.Name)
		s.managedJobs[config.Name] = config
		return
	}

	// CronJob 已存在且需要更新
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// 重新获取以避免冲突
		latest, err := s.client.BatchV1().CronJobs(s.namespace).Get(context.TODO(), config.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		// 更新关键字段但保留其他元数据
		cronJob.ObjectMeta.ResourceVersion = latest.ObjectMeta.ResourceVersion

		_, err = s.client.BatchV1().CronJobs(s.namespace).Update(context.TODO(), cronJob, metav1.UpdateOptions{})
		return err
	})

	if err != nil {
		log.Printf("更新 CronJob %s 失败: %v", config.Name, err)
		return
	}

	log.Printf("成功更新 CronJob: %s", config.Name)
	s.managedJobs[config.Name] = config
}

// 删除 CronJob
func (s *Scheduler) deleteCronJob(name string) {
	log.Printf("删除 CronJob: %s", name)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return s.client.BatchV1().CronJobs(s.namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	})

	if err != nil {
		if !errors.IsNotFound(err) {
			log.Printf("删除 CronJob %s 失败: %v", name, err)
			return
		}
	}

	log.Printf("成功删除 CronJob: %s", name)
	delete(s.managedJobs, name)
}

// 删除所有管理的 CronJob
func (s *Scheduler) deleteAllJobs() {
	for name := range s.managedJobs {
		s.deleteCronJob(name)
	}
}

// 构建 CronJob 对象
func (s *Scheduler) buildCronJobObject(config JobConfig) *batchv1.CronJob {
	// 构建容器
	container := corev1.Container{
		Name:    config.Name,
		Image:   config.Image,
		Command: config.Command,
		Args:    config.Args,
	}

	// 添加环境变量
	if len(config.Env) > 0 {
		container.Env = make([]corev1.EnvVar, 0, len(config.Env))
		for _, env := range config.Env {
			container.Env = append(container.Env, corev1.EnvVar{
				Name:  env.Name,
				Value: env.Value,
			})
		}
	}

	// 添加资源限制
	if config.Resources.Requests.CPU != "" || config.Resources.Requests.Memory != "" ||
		config.Resources.Limits.CPU != "" || config.Resources.Limits.Memory != "" {
		container.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{},
			Limits:   corev1.ResourceList{},
		}

		if config.Resources.Requests.CPU != "" {
			container.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(config.Resources.Requests.CPU)
		}
		if config.Resources.Requests.Memory != "" {
			container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(config.Resources.Requests.Memory)
		}
		if config.Resources.Limits.CPU != "" {
			container.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(config.Resources.Limits.CPU)
		}
		if config.Resources.Limits.Memory != "" {
			container.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(config.Resources.Limits.Memory)
		}
	}

	// 添加卷挂载
	if len(config.VolumeMounts) > 0 {
		container.VolumeMounts = make([]corev1.VolumeMount, 0, len(config.VolumeMounts))
		for _, vm := range config.VolumeMounts {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      vm.Name,
				MountPath: vm.MountPath,
				ReadOnly:  vm.ReadOnly,
			})
		}
	}

	// 构建 Pod 规格
	podSpec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyOnFailure,
		Containers:    []corev1.Container{container},
	}

	// 添加卷
	if len(config.Volumes) > 0 {
		podSpec.Volumes = make([]corev1.Volume, 0, len(config.Volumes))
		for _, v := range config.Volumes {
			volume := corev1.Volume{
				Name: v.Name,
			}

			switch v.Type {
			case "configMap":
				volume.ConfigMap = &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: v.ConfigMap,
					},
				}
			case "secret":
				volume.Secret = &corev1.SecretVolumeSource{
					SecretName: v.Secret,
				}
			case "emptyDir":
				volume.EmptyDir = &corev1.EmptyDirVolumeSource{}
			}

			podSpec.Volumes = append(podSpec.Volumes, volume)
		}
	}

	// 构建 CronJob 对象
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:        config.Name,
			Namespace:   s.namespace,
			Labels:      config.Labels,
			Annotations: config.Annotations,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: config.Schedule,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      config.Labels,
					Annotations: config.Annotations,
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      config.Labels,
							Annotations: config.Annotations,
						},
						Spec: podSpec,
					},
				},
			},
		},
	}

	return cronJob
}

// 检查 CronJob 是否需要更新
func (s *Scheduler) needsUpdate(existing *batchv1.CronJob, desired *batchv1.CronJob) bool {
	// 检查调度表达式
	if existing.Spec.Schedule != desired.Spec.Schedule {
		return true
	}

	// 检查容器配置
	if len(existing.Spec.JobTemplate.Spec.Template.Spec.Containers) == 0 {
		return true
	}

	existingContainer := existing.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	desiredContainer := desired.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	if existingContainer.Image != desiredContainer.Image {
		return true
	}

	if !reflect.DeepEqual(existingContainer.Command, desiredContainer.Command) {
		return true
	}

	if !reflect.DeepEqual(existingContainer.Args, desiredContainer.Args) {
		return true
	}

	if !reflect.DeepEqual(existingContainer.Env, desiredContainer.Env) {
		return true
	}

	if !reflect.DeepEqual(existingContainer.Resources, desiredContainer.Resources) {
		return true
	}

	if !reflect.DeepEqual(existingContainer.VolumeMounts, desiredContainer.VolumeMounts) {
		return true
	}

	// 检查卷配置
	if !reflect.DeepEqual(existing.Spec.JobTemplate.Spec.Template.Spec.Volumes, desired.Spec.JobTemplate.Spec.Template.Spec.Volumes) {
		return true
	}

	// 检查标签和注解
	if !reflect.DeepEqual(existing.ObjectMeta.Labels, desired.ObjectMeta.Labels) {
		return true
	}

	if !reflect.DeepEqual(existing.ObjectMeta.Annotations, desired.ObjectMeta.Annotations) {
		return true
	}

	return false
}

// 优雅停机
func setupGracefulShutdown(scheduler *Scheduler) {
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-signalCh
		log.Printf("接收到信号 %s，开始优雅停机...", sig)

		// 停止 ConfigMap 监听器
		close(scheduler.informerStopCh)
		// 停止主循环
		close(scheduler.stopCh)
	}()
}

// 获取 Kubernetes 客户端
func getKubernetesClient(kubeconfigPath string) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	// 尝试集群内配置
	config, err = rest.InClusterConfig()
	if err != nil {
		// 如果不在集群内，尝试使用 kubeconfig
		if kubeconfigPath == "" {
			// 如果环境变量未设置，使用默认路径
			if home := homedir.HomeDir(); home != "" {
				kubeconfigPath = filepath.Join(home, ".kube", "config")
			} else {
				return nil, fmt.Errorf("无法找到 kubeconfig：环境变量 KUBECONFIG 未设置且无法确定用户主目录")
			}
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, err
		}
	}

	config.Timeout = 30 * time.Second

	return kubernetes.NewForConfig(config)
}

// 从环境变量获取配置，如果不存在则使用默认值
func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

// 从环境变量获取整数配置
func getEnvAsInt(key string, defaultValue int) int {
	valueStr := getEnv(key, fmt.Sprintf("%d", defaultValue))

	var parsed int
	if _, err := fmt.Sscanf(valueStr, "%d", &parsed); err == nil {
		return parsed
	}

	log.Printf("警告: 无法解析环境变量 %s 的值 '%s' 为整数，使用默认值 %d", key, valueStr, defaultValue)
	return defaultValue
}
