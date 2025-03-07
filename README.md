## cronjob scheduler 启动文档

### 1. **准备 Kubernetes 集群**
确保您有一个可用的 Kubernetes 集群，并且可以通过 `kubectl` 访问它。

---

### 2. **构建调度器镜像**
将代码打包成 Docker 镜像并推送到镜像仓库。

#### Dockerfile 示例
```dockerfile
FROM golang:1.23.6 AS builder
WORKDIR /app
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o scheduler .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/scheduler .
EXPOSE 9991
CMD ["./scheduler"]
```

#### 构建并推送镜像
```bash
# 构建镜像
docker build -t bamboo/cronjob-scheduler:v1 .
```

---

### 3. **创建 ConfigMap**
在 Kubernetes 中创建一个 ConfigMap，用于存储 CronJob 的配置。

#### ConfigMap 示例
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cronjob-configs
  namespace: default
data:
  jobs.yaml: |
    jobs:
      - name: backup-job
        schedule: "0 1 * * *"
        image: backup-image:v1
        command: ["/bin/sh", "-c", "echo performing backup"]
        resources:
          requests:
            memory: "64Mi"
            cpu: "100m"
          limits:
            memory: "128Mi"
            cpu: "200m"
      - name: cleanup-job
        schedule: "0 3 * * *"
        image: cleanup-image:v1
        command: ["/bin/sh", "-c", "echo cleanup"]
```

#### 应用 ConfigMap
```bash
kubectl apply -f configmap.yaml
```

---

### 4. **部署调度器**
创建一个 Kubernetes Deployment 来运行调度器。

#### Deployment 示例
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cronjob-scheduler
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: cronjob-scheduler
  template:
    metadata:
      labels:
        app: cronjob-scheduler
    spec:
      serviceAccountName: cronjob-scheduler-sa
      containers:
      - name: scheduler
        image: bamboo/cronjob-scheduler:v1
        env:
        - name: NAMESPACE
          value: "default"
        - name: CONFIGMAP_NAME
          value: "cronjob-configs"
        - name: POLL_INTERVAL
          value: "30"
        resources:
          limits:
            memory: "128Mi"
            cpu: "100m"
```

#### 应用 Deployment
```bash
kubectl apply -f deployment.yaml
```

---

### 5. **配置 RBAC**
调度器需要访问 Kubernetes API 来管理 CronJob 和 ConfigMap，因此需要配置 RBAC。

#### RBAC 配置示例
```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cronjob-scheduler-sa
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cronjob-scheduler-role
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "watch", "list"]
- apiGroups: ["batch"]
  resources: ["cronjobs"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cronjob-scheduler-rb
subjects:
- kind: ServiceAccount
  name: cronjob-scheduler-sa
  namespace: default
roleRef:
  kind: ClusterRole
  name: cronjob-scheduler-role
  apiGroup: rbac.authorization.k8s.io
```

#### 应用 RBAC
```bash
kubectl apply -f rbac.yaml
```

---

### 6. **验证调度器**
调度器启动后，会自动根据 ConfigMap 中的配置创建或更新 CronJob。

#### 检查调度器日志
```bash
kubectl logs -l app=cronjob-scheduler
```

#### 检查 CronJob 状态
```bash
kubectl get cronjobs
```

---

### 7. **更新 ConfigMap**
当您更新 ConfigMap 中的 `jobs.yaml` 时，调度器会自动检测变化并同步 CronJob。

#### 示例更新
```yaml
data:
  jobs.yaml: |
    jobs:
      - name: backup-job
        schedule: "0 1 * * *"
        image: backup-image:v2  # 更新镜像版本
        command: ["/bin/sh", "-c", "echo performing backup"]
      - name: cleanup-job
        schedule: "0 3 * * *"
        image: cleanup-image:v1
        command: ["/bin/sh", "-c", "echo cleanup"]
```

#### 应用更新
```bash
kubectl apply -f configmap.yaml
```