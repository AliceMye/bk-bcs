# Helm Release 更新接口「可选是否注入 io.tencent.paas 系列注解」服务端整改方案

> 适用接口：`PUT /bcsapi/v4/helmmanager/v1/projects/{projectCode}/clusters/{clusterID}/namespaces/{namespace}/releases/{name}`
> 组件：bcs-helm-manager
> 文档日期：2026-10-09
> 已决策：开关需持久化；作用范围覆盖 `creator` / `updator` / `version` 三个注解，按仓库既有 `shouldRemoveAnnotations` 分组

---

## 一、需求背景

用户不希望 BCS 在其部署的资源上注入 `io.tencent.paas.updator` 注解。

现有机制是"只要在 BCS 内更新资源就会自动打上该注解"，该机制无法全局关闭，因为已有用户依赖它（控制台"最后更新人"、资源创建来源判断）。

因此方案是：**在部署/更新入口提供一个选项，让用户主动选择是否注入该注解**。前端负责勾选交互，服务端负责接收参数、持久化并控制注入逻辑。

**作用范围已确认为不局限于 `updator`**，而是覆盖 BCS 注入的三个 paas 系列注解（详见 6.1）。因此开关与相关字段统一命名为 `skipPaasAnnotations`，不再使用 `skipUpdatorAnnotation`——后者会误导后续维护者。

---

## 二、现状：注解是怎么被注入的

### 2.1 Helm 链路（本方案要改的）

注入不是直接写死注解，而是由 **patch 模板 + 占位符替换** 完成。

**第一步**：服务端在更新时把 `PTKCreator` / `PTKUpdator` / `PTKVersion` 等塞进 `PatchTemplateValues`。

文件：`internal/operation/actions/upgrade.go:219-228`

```go
PatchTemplateValues: map[string]string{
    common.PTKProjectID: r.projectID,
    common.PTKClusterID: r.clusterID,
    common.PTKNamespace: r.namespace,
    common.PTKCreator:   stringx.ReplaceIllegalChars(r.createBy),
    common.PTKUpdator:   stringx.ReplaceIllegalChars(r.updateBy),
    common.PTKVersion:   r.version,
    common.PTKName:      r.name,
},
```

**第二步**：这些 key 就是占位符。定义见 `internal/common/patch.go:36-47`，形如 `__BCS_HELM_PATCH_CREATOR__`。

**第三步**：Helm 渲染后走 PostRenderer 替换占位符。挂载点在 `internal/release/bcs/sdk/sdk.go:266`（upgrade）与 `:205`（install）。

**第四步**：替换逻辑。文件 `internal/release/bcs/sdk/patch.go:38-48`。

```go
func replacePatchTplKey(keys map[string]string, data []byte) []byte {
	for k, v := range keys {
		if !common.IsPatchTemplateKey(k) {
			continue
		}

		data = []byte(strings.ReplaceAll(string(data), k, v))
	}

	return common.EmptyAllPatchTemplateKey(data)
}
```

注意：`replacePatchTplKey` **只作用于 patch 模板**（见 `newPatcher`，`patch.go:50-59`），不作用于 chart 渲染结果。所以**生效的**注解一定来自 patch 模板。

⚠️ 但要注意区分两种占位符，实测中两种都出现过：

| 形态 | 来源 | 是否会被替换 | 实测出处 |
| --- | --- | --- | --- |
| `__BCS_HELM_PATCH_CREATOR__` | patch 模板 | ✅ 由 `replacePatchTplKey` 替换为真实值；未命中的被 `EmptyAllPatchTemplateKey` 抹空 | patch 模板（`release.patchdir`） |
| `{{ creator }}` / `{{ source_type }}` | **chart 自身** | ❌ 不会 —— 替换逻辑无此形态，`{{ }}` 也不是 ytt 语法 | Helm manifest |

实测 `helm get manifest bcs-services-stack -n bcs-system` 里能看到未被替换的 `{{ creator }}` / `{{ updator }}` / `{{ source_type }}` / `{{ project_id }}` / `{{ version }}`，说明 **chart 确实会写占位符，但这些是本方案要动的那份以外的死文本**。

结论：**chart 自己写占位符确实无效**（不会被替换），但也不能据此断言 manifest 里的 `io.tencent.paas.*` 都来自 patch 模板 —— 判断依据是"值是否为真实值"。真实值来自 patch 模板，未替换的 `{{ }}` 是 chart 的死文本。

**第五步**：patch 模板来自配置项 `release.patchdir`，服务启动时从磁盘加载。见 `internal/options/options.go:97-102` 与 `internal/app/app.go:300-311`。

### 2.2 三个注解是一组（主流做法依据）

现有代码已经把它们当一组处理。`internal/actions/release/utils.go:91-95`：

```go
var shouldRemoveAnnotations = []string{
	"io.tencent.paas.creator",
	"io.tencent.paas.updator",
	"io.tencent.paas.version",
}
```

这段代码用于"展示时剥掉这些注解"。**本方案的删除名单与常量定义直接对齐这里**，保持一致，不做 `creator` 豁免。

### 2.2.1 实测：注入位置的完整矩阵（2026-10-09 实测确认）

实测对象：Deployment `bcs-system/bcs-push-manager`（独立 release `bcs-push-manager`，只有一个 Deployment，噪音最小）。

数据来源三条：

1. `helm get manifest bcs-push-manager -n bcs-system` —— Helm 存储的 manifest（含 patch 模板注入结果）
2. `kubectl get deploy ... -o jsonpath='{.metadata.*}'`
3. `kubectl get deploy ... -o jsonpath='{.spec.template.metadata.*}'`

manifest 片段（真实值，非占位符）：

```yaml
metadata:
  labels:
    io.tencent.paas.source_type: helm
    io.tencent.paas.projectid: 7538606f025efa007f3e750477982c23
    io.tencent.paas.creator: hailinnie
    io.tencent.paas.updator: hailinnie
  annotations:
    io.tencent.paas.creator: hailinnie
    io.tencent.paas.updator: hailinnie
    io.tencent.paas.version: 1.31.0-alpha.83
```

**汇总矩阵**：

| key | `metadata.`<br>`annotations` | `metadata.`<br>`labels` | `spec.template.`<br>`metadata.annotations` | `spec.template.`<br>`metadata.labels` |
| --- | --- | --- | --- | --- |
| `io.tencent.paas.creator` | ✅ | ✅ | — | — |
| `io.tencent.paas.updator` | ✅ | ✅ | — | — |
| `io.tencent.paas.version` | ✅ | — | — | — |
| `io.tencent.paas.source_type` | — | ✅ | — | ✅ |
| `io.tencent.paas.projectid` | — | ✅ | — | ✅ |

由此得出三条必须在实现时遵守的事实：

**1. `creator` / `updator` 两处都有，`version` 只在 annotations。**

**labels 里那份极易漏掉** —— 只清理 annotations 的话，用户 `kubectl get deploy -o yaml` 仍然看得到人名，会认为功能没生效。所以清理必须同时覆盖 `metadata.annotations` 与 `metadata.labels`（详见改动 9）。

**2. 分布不对称，不能写一段通用逻辑无差别套用。**

`version` 没有 labels 那份，`source_type` / `projectid` 没有 annotations 那份。必须逐位置判断，路径不存在时跳过。

**3. `source_type` / `projectid` 不在本方案删除范围内。**

这两个是「来源 / 归属」元数据，与需求针对的「创建人 / 更新人」无关：

- `io.tencent.paas.source_type` 被 `bcs-services/cluster-resources/pkg/resource/formatter/common.go:76-79` 用于判断资源创建来源
- `io.tencent.paas.projectid` 是项目归属标识

只删「人」组三个（`creator` / `updator` / `version`），与 6.1 已决策的范围一致。

### 2.3 另一条链路（本方案不涉及）

`cluster-resources` 的表单/YAML 更新、工作负载重启，是直接写注解的：

- `pkg/resource/form/renderer/manifest.go:162-182`（表单渲染补全）
- `pkg/action/trans/transformer.go:75-90`（创建/更新写入）
- `pkg/action/resource/resource.go:223-231`（工作负载重启 patch）

与本接口无关。但这条链路写入的注解，Helm 侧删不掉（见风险 6）。

---

## 三、关键结论：「不放占位符」不等于「注解消失」

### 3.1 现象

假设 patch 模板原文是：

```yaml
metadata:
  annotations:
    io.tencent.paas.updator: __BCS_HELM_PATCH_UPDATOR__
```

当不放对应占位符时，兜底逻辑 `EmptyAllPatchTemplateKey` 会把占位符整体抹掉。见 `internal/common/patch.go:54-57`：

```go
func EmptyAllPatchTemplateKey(source []byte) []byte {
	return patchTemplateKeyRegex.ReplaceAll(source, nil)
}
```

各层结果：

- 源码文本层：`io.tencent.paas.updator:` —— 占位符变空串
- YAML 解析层：`{"io.tencent.paas.updator": null}`
- K8s 落库层：`io.tencent.paas.updator: ""`

最终用户看到的是 **键还在、值为空**，而不是整条注解消失。

### 3.2 根因

`EmptyAllPatchTemplateKey` 是**字节层的正则替换**，输入输出的都是 `[]byte`，完全不解析 YAML。它连"这是一个 annotation"都不知道。

所以它的能力上限只能是抹掉占位符那几个字符，**没有任何机制能删掉旁边的 YAML 键**。

另外，这个函数的**设计目的是兜底防泄漏**（防止原始 token 出现在最终 manifest 里），而不是"把注解设为空值"。所以"空串"根本不是一个服务端功能，而是兜底逻辑的副作用。

### 3.3 对 `creator` 尤其危险

**"不放占位符"会让 `creator` 的值被空串覆盖**，而 `creator` 是创建者信息，本应写入后不再变化：

- 期望：不注入 → `creator` 不被本次更新改动
- 实际（只不放占位符）：`creator` 被清空，原有创建者信息丢失

这比"注解还在"严重得多。所以三个注解都必须走"删除 key"路径，绝不能只靠"不放占位符"。

### 3.4 归属划分

- 服务端代码能控制的动作有两个：`PatchTemplateValues` 里放不放占位符、patcher 里删不删 key
- 之后是 `""` 还是 `null`，由 ytt / Helm / K8s 决定，服务端不参与
- 但"这些键到底存不存在"是需求目标，属于服务端要交付的东西

用户勾了「不注入」却看到 `io.tencent.paas.updator: ""`，会认为功能没生效。验收按用户视角。

---

## 四、实现方案选型

### 方案 A：只改一行 if

勾选时不放 `PTKCreator` / `PTKUpdator` / `PTKVersion`。

- 优点：改动极小
- 缺点：最终是三个空串字段，需求未达成；且 `creator` 会被清空

### 方案 B：开关驱动 + 无条件删除（采用）

上层不放三个占位符，同时在 `patcher.Run()` 里根据开关**无条件删掉三个目标 annotation 的 key**（不看值、不看 kind）。

优点：

- 覆盖所有模板写法，不需要预先知道 patch 模板长什么样
- patch 模板在部署环境（`release.patchdir`），不在本仓库，开发时拿不到，方案 B 不依赖它
- 结果确定，可测试

---

## 五、开关持久化（已决策：需要）

### 5.1 为什么必须持久化

HTTP 请求是无状态的，服务端每次只看到"这一次请求带了什么"。若不持久化：

1. 张三更新 release，勾了"不注入" → 请求带 `skip=true` → 不注入
2. 一周后李四更新同一 release，UI 默认未勾选 → 请求不带该字段 → 走默认 → **又注入了**
3. 更隐蔽的是 API / CI 调用：这类调用方不知道有这个开关，永远不传 → 永远注入

要让选择在多轮更新之间保持住，服务端必须"记住"它，而"记住"只能用 DB，即 `entity.Release`。

### 5.2 BCS 现有的同类做法

`upgradev1.go:162-170` 就是"持久化"的标准实现方式：请求没传 → 从 `old`（DB 记录）回填。

```go
if u.req.GetRepository() == "" {
    u.req.Repository = &old.Repo
}
```

### 5.3 bool 回填的坑（务必注意）

上述模式**不能直接套用到 bool 字段**。

`Repository` 等是 string，空串可以代表"没传"；但 bool 的零值 `false` 本身是有意义的（用户显式取消勾选）。如果写成：

```go
// 错误写法
if !u.req.GetSkipPaasAnnotations() {
    // 无法区分：是"用户显式取消勾选"，还是"根本没传"
}
```

结果就是**用户永远无法取消勾选**。

必须用指针判 nil：

```go
// 正确写法
if u.req.SkipPaasAnnotations == nil {
    u.req.SkipPaasAnnotations = &old.SkipPaasAnnotations
}
```

前提是 proto 里必须声明为 `optional bool`，才能生成 `*bool`。仓库里 `optional bool` 是标准写法（如 `takeover` / `remote` / `force`），符合要求。

---

## 六、改动前仍需与产品确认的点

### 6.1 已决策

**1. 开关是否持久化** —— 需要持久化到 `entity.Release`。理由见第五章。

**2. 作用范围** —— 覆盖三个 paas 系列注解，与 `internal/actions/release/utils.go:91-95` 现有分组一致：

- `io.tencent.paas.creator`
- `io.tencent.paas.updator`
- `io.tencent.paas.version`

**3. `creator` 不做豁免** —— 按主流做法三个一起删。代价是：install 时写入的 `creator` 会在后续"不注入"的 update 中被删除，属不可逆数据丢失。这一点需在 UI 文案中告知用户。

### 6.2 待确认（阻塞型）

**1. 默认行为**

必须是"注入"。否则老客户端全部变成"不注入"，直接破坏兼容。

**2. 是否允许取消**

用户勾了"不注入"之后，能否再改回"注入"？持久化后 UI 会回显，取消勾选应视为显式改回（依赖 5.3 的指针判断）。若产品要求不可逆，逻辑更简单，但 UI 要把选项置灰。

> 实现建议：**先按「允许取消」实现**，它是"不允许"的超集；若后续产品要求不可逆，加一层拒绝 + UI 置灰即可，反过来重建成本高。

### 6.2.1 已解决（原 6.2 第 3 条）：Helm 能删掉这些元数据 ✅

> 结论：**方案成立**。该项原为阻塞型，2026-10-09 实测完成，不再阻塞开发。

**背景**：实测 Deployment 的 managedFields 显示 `io.tencent.paas.updator`（annotations 与 labels 两处）由 `manager: bcs-cluster-resources` 管理。曾据此怀疑这些元数据一直在 Helm manifest 之外，从而 Helm 三路合并删不掉（`diff(old, new)` 不产生删除指令）。

**验证一：`helm get manifest` 里确实有这些 key，且是真实值**

```console
$ helm get manifest bcs-push-manager -n bcs-system | grep -n "io.tencent.paas"
12:          io.tencent.paas.source_type: helm
14:          io.tencent.paas.projectid: 7538606f025efa007f3e750477982c23
16:          io.tencent.paas.creator: hailinnie
18:          io.tencent.paas.updator: hailinnie
22:          io.tencent.paas.version: 1.31.0-alpha.83
```

命中 6.2 第 3 条的判定「**manifest 里有 → patch 模板注入生效，Helm 能删掉，方案成立**」。

补充说明：这些值显然是占位符被替换后的结果 —— 整条链路上只有 patch 模板的 `replacePatchTplKey`（`patch.go:38-48`）会做替换，`EmptyAllPatchTemplateKey` 会把残留 token 抹成空串。所以这些 key **由 patch 模板注入**，落在 Helm 管辖范围内，三路合并能生成删除操作。

**验证二：手工删除后没有任何一方写回**

```bash
# annotations
kubectl annotate deploy bcs-push-manager -n bcs-system \
  io.tencent.paas.creator- io.tencent.paas.updator- io.tencent.paas.version- --overwrite
# labels
kubectl label deploy bcs-push-manager -n bcs-system \
  io.tencent.paas.creator- io.tencent.paas.updator- --overwrite
```

两项 `--dry-run=server` 均通过，说明**没有准入 webhook 拦截**（若存在 webhook 自动写回，这里会失败）。

删除后跨多个时间窗口重复查询，`creator` / `updator` / `version` **始终没有回归**。managedFields 中 `bcs-cluster-resources` 的最后写入时间是 2026-01-08，距今 8 个月，未在观察期内产生新写入。

**结论**：

- 三个目标 key 可以由 Helm 链路删除
- 不存在持续的外部写回
- 本方案成立，可以按第七章清单实施

**遗留观察项**（不影响开工）：建议明天再复查一次上面两条 `kubectl get`，排除天级低频同步。

### 6.2.2 实测：pod template 干净，不会触发 Pod 滚动

```console
$ kubectl get deploy bcs-push-manager -n bcs-system \
    -o jsonpath='{.spec.template.metadata.annotations}{"\n"}{.spec.template.metadata.labels}{"\n"}'

{"app.kubernetes.io/instance":"bcs-push-manager","app.kubernetes.io/name":"bcs-push-manager",
 "io.tencent.bcs.clusterid":"BCS-K8S-00000","io.tencent.bcs.controller.name":"bcs-push-manager",
 "io.tencent.bcs.controller.type":"Deployment","io.tencent.bcs.namespace":"bcs-system",
 "io.tencent.paas.projectid":"7538606f025efa007f3e750477982c23","io.tencent.paas.source_type":"helm"}
```

第一行（pod template 的 annotations）**完全为空**；第二行（pod template 的 labels）只有 `source_type` / `projectid`，**没有 `creator` / `updator` / `version`**。

由此得出：

- 删除三个目标 key 不改变 pod template hash，**不会触发 Pod 滚动**
- 改动 9 中 `spec.template` 下的两条路径属防御性覆盖，路径不存在或无命中时自动跳过，保留即可
- patch 模板由 `app.go:302` 从 `release.patchdir` 全局加载，所有 release 共用同一套，故上述结论对所有 Helm release 成立

⚠️ 注意：`source_type` / `projectid` **确实在 pod template 的 labels 里**。本方案不删它们，所以无影响；若今后续范围扩大到这两个 key，**会直接触发 Pod 滚动**。

### 6.3 待确认（非阻塞，但影响验收）

**3. 已澄清：本次改动不会触发 Pod 滚动**

实测确认，当前 patch 模板把注解注入到了 Deployment 的 `metadata.annotations`，**没有注入到 `spec.template.metadata.annotations`**：

```console
$ kubectl get deploy bcs-platform-manager -n bcs-system \
    -o jsonpath='{.spec.template.metadata.annotations}'
{"checksum/config":"96644533...","kubectl.kubernetes.io/restartedAt":"2026-05-29T16:11:01+08:00",
 "kubectl.kubernetes.io/restartedVersion":"39"}
```

pod template 层只有 `checksum/config`（chart 自身用于控制 ConfigMap 变更后滚动）和 `kubectl rollout restart` 打的标记，**没有任何 `io.tencent.paas.*`**。

由此得出：

- 删除 paas 注解不改变 pod template hash，**不会触发 Pod 滚动**，UI 文案无需额外提示
- 改动 9 中第二个路径 `$.spec.template.metadata.annotations` 属防御性代码，路径不存在时自动跳过，保留即可
- patch 模板由 `app.go:302` 从 `release.patchdir` 全局加载，所有 release 共用同一套，故上述结论对所有 Helm release 成立

**4. 存量怎么处理**

勾了之后，之前由 Helm 注入过的会被三路合并删掉；但非 Helm 途径（BCS 表单/YAML 链路）写进去的，Helm 删不掉。是否配套做一次性清理任务？

**5. 入口范围**

只 `install` + `upgrade`，还是连 `rollback`、`preview` 一起？preview 不同步会出现"预览有、实际没有"。

**6. UI 文案与位置**

放在"Helm部署参数"tab？文案需明确告知会同时影响创建者/更新者/版本三个注解，且不可恢复。

---

## 七、服务端改动清单（含注释）

### 实施进度（2026-10-09）

前置验证已全部完成，`6.2` 的三个阻塞项均已关闭或降级为非阻塞。以下改动 **1～11 的代码部分已落地**，源码均已 `gofmt` 通过。

未完成项见下方「还差什么」，根因只有一个：`.pb.go` 尚未重新生成。

#### 还差什么（按先后顺序）

1. **重新生成协议代码** —— 在 `proto/bcs-helm-manager/` 下执行 `make`。

   该目标的完整链路见 `proto/bcs-helm-manager/Makefile`：先用 `docker build` 构建 proto 镜像，再用容器内的 protoc 生成四个文件，最后 `sync-to-bcsapi` 把生成物复制到 `bcs-common/pkg/bcsapi/helmmanager/`。

   ```bash
   cd bcs-services/bcs-helm-manager/proto/bcs-helm-manager && make
   ```

2. **编译验证** —— 生成后执行 `go build ./...`。在此之前会报如下错误，均属预期，**不是代码缺陷**：

   | 报错位置 | 内容 | 原因 |
   | --- | --- | --- |
   | `entity/release.go` | `Unknown field 'SkipPaasAnnotations' in struct literal` | `ReleaseDetail` 尚未包含新字段 |
   | `upgradev1.go` | `Unresolved reference 'GetSkipPaasAnnotations'` | `UpgradeReleaseV1Req` 的 getter 尚未生成 |
   | `installv1.go` | 同上 | `InstallReleaseV1Req` 的 getter 尚未生成 |

3. **别忘了同步** —— `make` 的 `sync-to-bcsapi` 步骤会把生成物复制到 `bcs-common/pkg/bcsapi/helmmanager/`。跳过这一步会导致调用方（含 gateway / 其他服务）拿不到新字段。

#### 改动范围提醒

`removePaasMetadata` 只处理 `metadata` 级别的 annotations 与 labels。`spec.template` 下的两条路径是防御性覆盖（实测未注入该层），保留是为了兼容将来模板调整。

---

### 改动 1｜协议层

文件：`proto/bcs-helm-manager/bcs-helm-manager.proto`

三处 message 各加一个字段：

```protobuf
message UpgradeReleaseV1Req {
    // ... 已有字段 1-12 ...

    // 是否不注入 BCS 的 io.tencent.paas 系列注解（creator/updator/version）
    // 默认 false，即保持原有注入行为，避免影响已有调用方
    // 必须用 optional，才能生成 *bool 用于区分"未传"与"显式传 false"
    optional bool skipPaasAnnotations = 13[(grpc.gateway.protoc_gen_swagger.options.openapiv2_field) = {
        title: "skipPaasAnnotations",
        description: "是否不注入 BCS 的 io.tencent.paas 系列注解，默认 false（注入）"
    }];
}
```

```protobuf
message InstallReleaseV1Req {
    // ... 已有字段 1-12 ...

    // 同 UpgradeReleaseV1Req，安装时也支持该开关
    optional bool skipPaasAnnotations = 13[(grpc.gateway.protoc_gen_swagger.options.openapiv2_field) = {
        title: "skipPaasAnnotations",
        description: "是否不注入 BCS 的 io.tencent.paas 系列注解，默认 false（注入）"
    }];
}
```

```protobuf
message ReleaseDetail {
    // ... 已有字段 1-19 ...

    // 返回当前 release 的开关状态，供前端更新页回显上次的勾选
    optional bool skipPaasAnnotations = 20[(grpc.gateway.protoc_gen_swagger.options.openapiv2_field) = {
        title: "skipPaasAnnotations",
        description: "是否不注入 BCS 的 io.tencent.paas 系列注解"
    }];
}
```

注意：字段必须**反向命名**。若写成 `injectPaasAnnotations`，所有未传该字段的老客户端会瞬间变成"不注入"。

改完执行既有代码生成流程（pb.go / validate / swagger）。

### 改动 2｜注解名常量

文件：`internal/common/patch.go`

与 `PTK*` 常量放在一起：

```go
// BCS 注入的 paas 系列元数据 key，与 internal/actions/release/utils.go 中的
// shouldRemoveAnnotations 保持一致，修改时请同步。
//
// 注意：实测这些 key 会同时出现在 annotations 和 labels 两处
// （Deployment 的 metadata.labels 里也有 io.tencent.paas.updator），
// 因此命名用 Metadata 而不是 Annotation。
var PaasMetadataKeys = []string{
	"io.tencent.paas.creator",
	"io.tencent.paas.updator",
	"io.tencent.paas.version",
}
```

### 改动 3｜实体字段

文件：`internal/store/entity/release.go`

```go
type Release struct {
	// ... 已有字段 ...
	Env                  string `json:"env" bson:"env"`
	// SkipPaasAnnotations 为 true 时，部署/更新不在资源中注入 BCS 的 io.tencent.paas 系列注解。
	// 存量记录该字段为零值 false，语义正好是"注入"，与现状一致，无需数据迁移。
	SkipPaasAnnotations bool `json:"skipPaasAnnotations" bson:"skipPaasAnnotations"`
}
```

同文件 `Transfer2DetailProto` 增加一行，让详情接口能回显：

```go
func (r *Release) Transfer2DetailProto() *helmmanager.ReleaseDetail {
	return &helmmanager.ReleaseDetail{
		// ... 已有字段 ...
		ValueFile:           common.GetStringP(r.ValueFile),
		// 回显开关状态，否则前端每次打开更新页都显示"未勾选"
		SkipPaasAnnotations: common.GetBoolP(r.SkipPaasAnnotations),
	}
}
```

### 改动 4｜DB 更新 key

文件：`internal/store/entity/keys.go`

```go
const (
	// ... 已有 key ...
	FieldKeyEnv = "env"
	// FieldKeySkipPaasAnnotations 是否不注入 paas 系列注解
	FieldKeySkipPaasAnnotations = "skipPaasAnnotations"
)
```

### 改动 5｜更新接口入参与持久化

文件：`internal/actions/release/upgradev1.go`

**5.1** `upgrade()` 构建 Option 时透传（第 98-116 行）：

```go
	options := &actions.ReleaseUpgradeActionOption{
		// ... 已有字段 ...
		IsShardCluster: cls.IsShared,
		// 透传开关，由后续 patcher 决定是否删除 paas 系列注解
		SkipPaasAnnotations: u.req.GetSkipPaasAnnotations(),
	}
```

**5.2** `saveDB()` 回填（第 162-170 行区域）：

```go
		if u.req.GetVersion() == "" {
			u.req.Version = &old.ChartVersion
		}
		// 未显式传该字段时，沿用 DB 中上一次的选择。
		// 注意必须用指针判 nil，不能用 GetSkipPaasAnnotations()：
		// bool 的零值 false 本身有意义（用户显式取消勾选），
		// 用零值判断会导致用户无法取消勾选。
		if u.req.SkipPaasAnnotations == nil {
			u.req.SkipPaasAnnotations = &old.SkipPaasAnnotations
		}
```

**5.3** `saveDB()` 写入（第 171-182 行区域）：

```go
		rl := entity.M{
			// ... 已有字段 ...
			entity.FieldKeyEnv:          u.req.GetEnv(),
			// 持久化开关，保证后续未传该字段的调用（含 API/CI）仍保持用户的选择
			entity.FieldKeySkipPaasAnnotations: u.req.GetSkipPaasAnnotations(),
		}
```

**5.4** `saveDB()` 的 create 分支（`release` 不存在时创建，第 142-156 行）同样带上：

```go
		if err = u.model.CreateRelease(u.ctx, &entity.Release{
			// ... 已有字段 ...
			Env:                  u.req.GetEnv(),
			SkipPaasAnnotations:  u.req.GetSkipPaasAnnotations(),
		}); err != nil {
```

### 改动 6｜异步动作透传

文件：`internal/operation/actions/upgrade.go`

三处结构体/构造加字段：

```go
type ReleaseUpgradeAction struct {
	// ... 已有字段 ...
	IsShardCluster bool

	// skipPaasAnnotations 为 true 时不注入 BCS 的 io.tencent.paas 系列注解
	skipPaasAnnotations bool

	contents []byte
	result   *release.HelmUpgradeResult
}

type ReleaseUpgradeActionOption struct {
	// ... 已有字段 ...
	IsShardCluster bool

	// SkipPaasAnnotations 同 ReleaseUpgradeAction.skipPaasAnnotations
	SkipPaasAnnotations bool
}

func NewReleaseUpgradeAction(o *ReleaseUpgradeActionOption) *ReleaseUpgradeAction {
	return &ReleaseUpgradeAction{
		// ... 已有字段 ...
		IsShardCluster:      o.IsShardCluster,

		skipPaasAnnotations: o.SkipPaasAnnotations,
	}
}
```

**关键改动** —— `PatchTemplateValues` 构造（第 219-228 行）：

```go
		// 基础占位符始终注入
		patchValues := map[string]string{
			common.PTKProjectID: r.projectID,
			common.PTKClusterID: r.clusterID,
			common.PTKNamespace: r.namespace,
			common.PTKName:      r.name,
		}
		// 用户选择不注入时才跳过 paas 系列占位符。
		// 注意：仅跳过占位符是不够的——patch 模板中的注解行会变成空串而非消失，
		// 还会把 creator 原有值覆盖为空。因此必须配合改动 9 的 removePaasAnnotations
		// 把注解 key 整条删除。
		if !r.skipPaasAnnotations {
			patchValues[common.PTKCreator] = stringx.ReplaceIllegalChars(r.createBy)
			patchValues[common.PTKUpdator] = stringx.ReplaceIllegalChars(r.updateBy)
			patchValues[common.PTKVersion] = r.version
		}
```

然后把 `PatchTemplateValues: patchValues` 填入 `HelmUpgradeConfig`。

### 改动 7｜配置结构体

文件：`internal/release/release.go`

```go
type HelmUpgradeConfig struct {
	// ... 已有字段 ...
	PatchTemplateValues map[string]string

	// SkipPaasAnnotations 为 true 时不注入 BCS 的 io.tencent.paas 系列注解
	SkipPaasAnnotations bool
}

func (h *HelmUpgradeConfig) ToInstallConfig() HelmInstallConfig {
	return HelmInstallConfig{
		// ... 已有字段 ...
		PatchTemplateValues: h.PatchTemplateValues,
		// upgrade 复用 install 链路，必须一并传递
		SkipPaasAnnotations: h.SkipPaasAnnotations,
	}
}
```

`HelmInstallConfig`（第 205-216 行）同样加该字段。

### 改动 8｜PostRenderer 传参

文件：`internal/release/bcs/sdk/sdk.go`

```go
	// install（第 205 行）
	installer.PostRenderer = newPatcher(c.group.config.PatchTemplates, config.PatchTemplateValues,
		config.SkipPaasAnnotations)

	// upgrade（第 266 行）
	upgrader.PostRenderer = newPatcher(c.group.config.PatchTemplates, config.PatchTemplateValues,
		config.SkipPaasAnnotations)
```

### 改动 9｜核心删除逻辑

文件：`internal/release/bcs/sdk/patch.go`

**9.1** `newPatcher` 与 `patcher` struct：

```go
// newPatcher 构建 post-render patcher
// skipPaasAnnotations 为 true 时，会在渲染后删除 BCS 注入的 paas 系列注解
func newPatcher(templates []*release.File, keys map[string]string, skipPaasAnnotations bool) *patcher {
	fs := make([]*files.File, 0, 5)
	for _, f := range templates {
		fs = append(fs, files.MustNewFileFromSource(files.NewBytesSource(f.Name, replacePatchTplKey(keys, f.Content))))
	}

	return &patcher{
		files:               fs,
		skipPaasAnnotations: skipPaasAnnotations,
	}
}

type patcher struct {
	files []*files.File
	// skipPaasAnnotations 为 true 时删除 io.tencent.paas 系列注解
	skipPaasAnnotations bool
}
```

**9.2** `Run()` 中调用（第 77-88 行循环内）：

```go
	for _, s := range splitManifests {
		// 向 metadata.labels / spec.template.metadata.labels 中注入 `io.tencent.bcs.controller.name`
		var j string
		j, err = inject4MetadataLabels(s)
		if err != nil {
			blog.Errorf("inject4MetadataLabels error, %s", err.Error())
			yList = append(yList, s)
			continue
		}

		// 用户选择不注入时，删除 io.tencent.paas 系列元数据（annotations + labels）
		if p.skipPaasAnnotations {
			j, err = removePaasMetadata(j)
			if err != nil {
				blog.Errorf("removePaasMetadata error, %s", err.Error())
				yList = append(yList, s)
				continue
			}
		}

		yList = append(yList, strings.TrimRight(j, "\n"))
	}
```

**9.3** 新增删除函数（与 `inject4MetadataLabels` 对称）：

```go
// paasMetaPaths 需要清理 paas 元数据的位置。
// annotations 与 labels 都要清理：实测 Deployment 的 metadata.annotations 与
// metadata.labels 两处都有 io.tencent.paas.updator，只清 annotations 会漏。
// spec.template 下的两份是防御性覆盖（实测当前模板未注入该层），
// 路径不存在时 removePaasMetadataByPath 会自动跳过，无副作用。
var paasMetaPaths = []string{
	"$.metadata.annotations",
	"$.metadata.labels",
	"$.spec.template.metadata.annotations",
	"$.spec.template.metadata.labels",
}

// removePaasMetadata 删除 BCS 注入的 io.tencent.paas 系列元数据（annotations 与 labels）。
// 必须与"不放占位符"配合使用：仅不放占位符会让值变成空串而不是消失，
// 且会把 creator 原有值覆盖为空，造成不可逆的数据丢失。
// 未删到任何内容时原样返回 manifest，避免对无关资源做无谓的重新格式化。
func removePaasMetadata(manifest string) (string, error) {
	// 转换为常用格式, goyaml 库不能识别 |2- 之类的描述符
	var n yaml.MapSlice
	if err := yaml.Unmarshal([]byte(manifest), &n); err != nil {
		return manifest, err
	}
	out, err := yaml.Marshal(&n)
	if err != nil {
		return manifest, err
	}
	s := string(out)

	f, err := parser.ParseBytes([]byte(s), 0)
	if err != nil {
		return manifest, err
	}

	removed := false
	for _, path := range paasMetaPaths {
		ok, err := removePaasMetadataByPath(f, s, path)
		if err != nil {
			return manifest, err
		}
		if ok {
			removed = true
		}
	}

	// 与 inject4MetadataLabels 保持一致的收尾原则：
	// 没有命中就返回原始 manifest，不对无关资源做无谓的重新格式化
	if !removed {
		return manifest, nil
	}
	return f.String(), nil
}

// removePaasMetadataByPath 删除指定路径下属于 paas 系列的 key，返回是否真的删到了
func removePaasMetadataByPath(f *ast.File, s, path string) (bool, error) {
	p, err := goyaml.PathString(path)
	if err != nil {
		return false, err
	}

	items := make(map[string]string)
	// 路径不存在时直接跳过（如 ConfigMap 没有 spec.template）
	if err := p.Read(strings.NewReader(s), &items); err != nil {
		return false, nil
	}

	removed := false
	for k := range items {
		if stringx.StringInSlice(k, common.PaasMetadataKeys) {
			delete(items, k)
			removed = true
		}
	}
	if !removed {
		return false, nil
	}

	// 全部删空时写入空 map，避免出现 `annotations: []` / `labels: []` 导致 K8s 校验失败
	b, err := yaml.Marshal(items)
	if err != nil {
		return false, err
	}
	if err := p.ReplaceWithReader(f, bytes.NewReader(b)); err != nil {
		return false, err
	}
	return true, nil
}
```

需补充 import：

```go
	"github.com/goccy/go-yaml/ast"
```

（`bytes` / `strings` / `yaml` / `goyaml` / `parser` / `common` / `stringx` 该文件已引入。）

### 改动 10｜install 侧同步

与改动 5、6 对应：

- `internal/actions/release/installv1.go` —— 透传 `SkipPaasAnnotations`
- `internal/operation/actions/install.go` —— struct / Option / 构造函数加字段；`PatchTemplateValues`（第 219-227 行）同改动 6 的写法。注意 install 时 `createBy` 与 `updateBy` 都是 `r.username`
- `internal/release/release.go` 的 `HelmInstallConfig`（第 205-216 行）—— 加字段

### 改动 11｜一致性核查（必须确认）

`internal/release/release.go` 的 `340-355` 与 `376-390` 两个辅助函数也拼了 `PatchTemplateValues`（含 `PTKCreator` / `PTKUpdator` / `PTKVersion`）。需确认它们对应哪些入口（可能是 preview / rollback），并决定是否同步：

- **preview** —— `internal/actions/release/preview.go`，若走同一 patcher 则自动生效；不同步会出现"预览有注解、实际没有"
- **rollback** —— `internal/actions/release/rollbackv1.go`，回滚是 helm 回退 revision，可能不走 patcher

---

## 八、验证清单

**前置：先确认验证对象的归属（Release ≠ Deployment）**

Release 是 Helm 的发布单元，一个 Release 包含**多个** K8s 资源；Deployment 只是其中之一。所以：

- 在 Release 列表里搜 Deployment 名字搜不到，属正常现象，不代表该资源未被 Helm 管理（有 `meta.helm.sh/*` 注解即为 Helm 管理）
- `helm get manifest` 必须传 **Release 名**，不是 Deployment 名

```bash
# 看每个 Deployment 归哪个 Release
kubectl get deploy -n <ns> \
  -o custom-columns='DEPLOY:.metadata.name,RELEASE:.metadata.annotations.meta\.helm\.sh/release-name,CHART:.metadata.labels.helm\.sh/chart'
```

实例：Deployment `bcs-system/bcs-helm-manager` 的 `meta.helm.sh/release-name` 是 `bcs-services-stack`，所以在 Release 列表里按 `bcs-helm-manager` 搜不到。

1. **不传开关**（老调用方式）→ 与现状完全一致，三个注解都有值
2. **开关 = false** → 同上
3. **开关 = true** → 最终 manifest 里 `io.tencent.paas.creator` / `updator` / `version` 三条均不存在
4. 场景 3 部署后 `kubectl get -o yaml` → 看不到对应 key
5. **专项：`creator` 不被清空** → 任何情况下都不能出现 `io.tencent.paas.creator: ""`
6. **专项：注解不要变成 `annotations: []`** → 全删空时应为 `annotations: {}`，否则 K8s 校验失败
7. 场景 3 反复执行 → 稳定、可重复
8. **持久化验证**：勾了之后，用不带该字段的 API 请求再更新一次 → 仍然不注入
9. **取消持久化验证**：显式传 `false` 更新 → 恢复注入
10. **详情接口** → 返回 `skipPaasAnnotations`，UI 正确回显
11. 预览 vs 实际 → manifest 必须一致
12. Deployment / StatefulSet → 本开关不应触发任何 Pod 滚动（实测 paas 注解不在 pod template 中）
13. ConfigMap / Secret / CRD → YAML 格式未被无谓改写，diff 干净
14. 控制台"最后更新人"、"创建者" → 降级显示 `--`，不报错

---

## 九、风险与注意事项

**1. 字段正向命名**

使用 `injectPaasAnnotations` 之类的正向命名会导致所有老客户端变为"不注入"。必须反向命名。

**2. bool 回填用零值判断**

现有 `if u.req.GetXxx() == ""` 的模式只适用于 string。bool 字段必须用指针判 nil，否则用户无法取消勾选。

**3. `creator` 被清空**

只"不放占位符"而不删除 key，会让 `creator` 的值变成空串且不可逆。必须走删除路径。验证清单第 5 条专门覆盖。

**4. 删除即不可逆**

Helm 三路合并下，"不注入"等价于"删除"，不存在中间态。一旦删除，注解值无法从集群侧找回（除非重新注入）。按主流做法三个都删，意味着 `creator` 也会丢失，需在 UI 文案中告知。

**5. YAML round-trip 波及非目标 kind**

若对所有文档都做 unmarshal / marshal，CRD / Secret 等会出现大面积无意义 diff。必须只在命中删除时才返回改写结果。

**6. preview 与 execute 不同步**

用户看到的预览和实际下发不一致，属于高发问题。必须确认改动 11。

**7. Helm 可能删不掉非 Helm 链路的元数据（重要）**

本次改动只覆盖 Helm install / upgrade 链路，但 updator 元数据有多个写入来源。实测 Deployment 的 managedFields：

```yaml
- manager: bcs-cluster-resources
  operation: Update
  fieldsV1:
    f:metadata:
      f:annotations:
        f:io.tencent.paas.updator: {}   # ← 它写的
      f:labels:
        f:io.tencent.paas.updator: {}   # ← 它也写 labels
    f:spec:
      f:template:
        f:spec:
          f:containers:
            k:{"name":"bcs-helm-manager"}:
              f:image: {}
              f:imagePullPolicy: {}
```

说明这条元数据由 `cluster-resources` 写入（控制台的更新/重启操作），而非 helm-manager。

由此产生两个问题：

**问题一：rollout restart 会把元数据写回来（仍然成立）**

1. 用户勾了"不注入"，Helm 更新后元数据消失
2. 有人在控制台点了"重启"（`pkg/action/resource/resource.go:223-231`）→ 元数据又被写回去
3. 直到下一次 Helm upgrade 才会再次被删掉

实测该 release 的 `kubectl.kubernetes.io/restartedVersion: 39`，重启非常频繁，该场景大概率会发生。

这是本方案**唯一的残留缺口**，但与其余部分是并发场景而非阻塞项 —— 用户勾了开关后仍能通过 Helm upgrade 把元数据删掉，只是被"重启"插队时会短暂回归。已在 6.2.1 实测确认：正常情况下不存在持续写回。

若要彻底解决，`cluster-resources` 侧也需同步感知该开关，超出本次改动范围。

**问题二：Helm 三路合并能不能删掉它（已解决 ✅）**

原担心点：Helm 的规则是"只有上一次 manifest 里存在、本次不存在的字段才会被删除"，若 updator 一直在 Helm manifest 之外，`diff(old, new)` 不会产生删除指令。

2026-10-09 实测结论：**能删掉**。两项验证见 6.2.1 —— `helm get manifest` 里存在真实值，且手工删除后无任何一方写回。不再阻塞。

**8. 存量注解清理边界**

Helm 三路合并只能删除"之前由 Helm 注入过"的注解。非 Helm 途径（`cluster-resources` 的表单 / YAML / rollout restart 链路）写入的无法删除，需要独立的清理任务。

**9. 数据库兼容**

`entity.Release` 新增字段时，存量记录该字段为零值 `false`，语义正好是"注入"，与现状一致，无需数据迁移。

---

## 十、附录：关键代码位置索引

- 更新接口入口 —— `internal/actions/release/upgradev1.go`
- 安装接口入口 —— `internal/actions/release/installv1.go`
- 更新异步动作 —— `internal/operation/actions/upgrade.go`
- 安装异步动作 —— `internal/operation/actions/install.go`
- 安装/更新配置结构 —— `internal/release/release.go`
- Release 实体定义 —— `internal/store/entity/release.go:25-44`
- 实体字段 key 常量 —— `internal/store/entity/keys.go:36-46`
- PostRenderer 挂载 —— `internal/release/bcs/sdk/sdk.go:205`（install）、`:266`（upgrade）
- 占位符替换与注入 —— `internal/release/bcs/sdk/patch.go`
- 占位符常量 —— `internal/common/patch.go`
- 注解名分组（主流做法依据）—— `internal/actions/release/utils.go:91-95`
- patch 模板目录配置 —— `internal/options/options.go:97-102`
- patch 模板加载 —— `internal/app/app.go:300-311`
- 协议定义 —— `proto/bcs-helm-manager/bcs-helm-manager.proto`
- 注解读取（读侧）—— `bcs-services/cluster-resources/pkg/resource/formatter/common.go`
