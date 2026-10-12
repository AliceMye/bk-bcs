# skipPaasAnnotations 服务端实现总结与工作日报

> 需求：Helm Release 安装/更新时，允许用户选择是否注入 `io.tencent.paas` 系列注解
> 组件：`bcs-helm-manager`
> 文档日期：2026-10-10
> 当前状态：**服务端编码完成，待部署联调**
> 关联文档：`docs/updator-annotation-optout.md`（需求背景与设计方案）

---

## 一、需求概述

BCS 在部署/更新资源时会自动注入 `io.tencent.paas.creator` / `updator` / `version` 三个注解。该行为无法全局关闭（已有用户依赖其做「最后更新人」「资源来源」展示），因此改为**在部署入口提供开关**，由用户主动选择。

| 项 | 内容 |
|---|---|
| 开关名 | `skipPaasAnnotations` |
| 语义 | `true` = 不注入；`false`/未传 = 注入（默认） |
| 作用范围 | `creator` / `updator` / `version` 三个注解，`annotations` + `labels` 两处 |
| 覆盖接口 | `InstallReleaseV1`、`UpgradeReleaseV1`、`ReleasePreview`、`GetReleaseDetailV1` |

---

## 二、改动清单

共 **18 个文件**（`docs/` 目录为新增未跟踪）。下表按分层列出，`git diff -w` 行数为忽略空白后的真实改动量。

### 2.1 协议层与生成产物

| 文件 | 改动 | 说明 |
|---|---|---|
| `proto/bcs-helm-manager/bcs-helm-manager.proto` | 12 | 新增 3 个 `optional bool` 字段 |
| `proto/.../bcs-helm-manager.pb.go` | 1176 | 重新生成（`*bool` 指针 + FileDescriptor 重排） |
| `proto/.../bcs-helm-manager.pb.validate.go` | 6 | 重新生成 |
| `proto/.../bcs-helm-manager.swagger.json` | 18 | 重新生成，前端接口契约 |
| `bcs-common/pkg/bcsapi/helmmanager/bcs-helm-manager.pb.go` | — | 同步（跨服务共享包） |
| `bcs-common/.../bcs-helm-manager.pb.validate.go` | — | 同步 |

新增字段：

| message | 字段号 | 用途 |
|---|---|---|
| `InstallReleaseV1Req` | 13 | 安装时开关入参 |
| `UpgradeReleaseV1Req` | 13 | 更新时开关入参 |
| `ReleaseDetail` | 20 | 详情回显，供更新页回显上次勾选 |

### 2.2 数据层

| 文件 | 真实改动 | 说明 |
|---|---|---|
| `internal/store/entity/release.go` | — | `Release` 结构体新增字段；`Transfer2DetailProto` 增加回显 |
| `internal/store/entity/keys.go` | 4 | 新增 `FieldKeySkipPaasAnnotations` 常量 |

### 2.3 API 层

| 文件 | 表面/真实 | 说明 |
|---|---|---|
| `internal/actions/release/installv1.go` | 60 / **2** | Option 传参 + `saveDB` 落库 |
| `internal/actions/release/upgradev1.go` | 86 / **6** | Option 传参 + 落库 + **指针回填** |
| `internal/actions/release/detailv1.go` | 1 / 1 | `mergeRelease` 回显开关 |
| `internal/actions/release/preview.go` | 10 / 10 | 预览取持久化的开关值 |

> 说明：installv1 / upgradev1 表面行数大是因为新增字段名 `SkipPaasAnnotations` 比原最长字段更长，gofmt 需重新对齐整个 struct literal，**纯空格变化，无逻辑变动**。

### 2.4 操作层（Operation 实现）

| 文件 | 真实改动 | 说明 |
|---|---|---|
| `internal/operation/actions/install.go` | 37 | 新增私有字段 + Option + `Validate`/`Execute` 使用 |
| `internal/operation/actions/upgrade.go` | 40 | 同上 |
| `internal/operation/actions/preview.go` | 11 | 预览保持与实际一致 |

框架层 `internal/operation/operation.go` **零改动** —— 它面向 `Operation` 接口调用 `Prepare`/`Validate`/`Execute`/`Done`，开关在实现内部，框架无需感知。

### 2.5 执行层（真正控制注入）

| 文件 | 真实改动 | 说明 |
|---|---|---|
| `internal/release/release.go` | 71 | `InstallRelease`/`UpgradeRelease` 新增入参，条件构建 `PatchTemplateValues` |
| `internal/release/bcs/sdk/sdk.go` | 6 | `newPatcher` 透传开关（Install + Upgrade） |
| `internal/release/bcs/sdk/patch.go` | 114 | 新增 `removePaasMetadata` / `removePaasMetadataByPath` / `paasMetaPaths` |
| `internal/common/patch.go` | 16 | 新增 `PaasMetadataKeys` 常量 |

---

## 三、关键设计决策

### 3.1 反向命名 `skip*`，而非 `inject*`

若正向命名 `injectPaasAnnotations`，所有未传该字段的老客户端会因零值 `false` 瞬间变成「不注入」，**直接破坏兼容性**。反向命名使默认值天然等于现状（注入）。

### 3.2 必须 `optional`，生成 `*bool`

用于区分：

- `nil` —— 用户没传（更新时继承旧值）
- `false` —— 用户显式取消勾选

`upgradev1.go` 依赖此实现指针回填：

```go
if u.req.SkipPaasAnnotations == nil {
    u.req.SkipPaasAnnotations = &old.SkipPaasAnnotations
}
```

生成产物已验证为 `SkipPaasAnnotations *bool`（依赖 `protoc-gen-go v1.3.2`，**不可升级**，新版会生成普通 `bool` 导致该逻辑失效）。

### 3.3 必须「双管齐下」：不放占位符 + patcher 删除 key

仅跳过占位符是不够的 —— patch 模板里的注解行会变成**空串而非消失**，且会把 `creator` 原有值覆盖为空，造成不可逆数据丢失。因此必须配合 `removePaasMetadata` 把注解 key 整条删除。

### 3.4 清理位置覆盖四处

```go
var paasMetaPaths = []string{
    "$.metadata.annotations",
    "$.metadata.labels",
    "$.spec.template.metadata.annotations",
    "$.spec.template.metadata.labels",
}
```

实测 `creator` / `updator` 同时存在于 `annotations` 与 `labels`，只清 `annotations` 会漏。`spec.template` 两份为防御性覆盖，路径不存在时自动跳过，无副作用、不触发 Pod 滚动。

### 3.5 三处取值方式刻意不同

| 位置 | 取值方式 | 原因 |
|---|---|---|
| install | `GetSkipPaasAnnotations()`（返回 `bool`） | 无旧值可继承，未传即 `false`，兼容性正确 |
| upgrade | `u.req.SkipPaasAnnotations`（`*bool`） | 需区分「未传」与「传 false」 |
| preview | 从 DB 读持久化值 | 保证预览与实际下发一致 |

### 3.6 预览/校验与执行保持一致

`Validate`（dry-run）、`Preview`、`Execute` 三处必须传同一个开关值，否则会出现「预览有注解、实际没有」。

---

## 四、验证结果

| 检查项 | 结果 |
|---|---|
| pb.go 生成 `*bool`（3 处） | ✅ 行 3409 / 3896 / 4240 |
| `bcs-common` 同步 | ✅ 与 proto 目录完全一致 |
| swagger.json 含新参数 | ✅ 18 行 |
| `protoc-gen-go` 版本未被升级 | ✅ v1.3.2 |
| Dockerfile | ✅ 已还原为原版（环境适配改动已回滚，插件版本未变，不影响产物） |
| `go build` | 待最终确认 |

---

## 五、待办与风险

| # | 事项 | 级别 | 说明 |
|---|---|---|---|
| 1 | **前端勾选框** | **阻塞** | 后端已就绪，前端无入口则功能不可用；需控制台更新页加选项 |
| 2 | `docs/` 目录未跟踪 | 低 | 需决定提交或删除 |
| 3 | 新增逻辑无单测 | 中 | `removePaasMetadata` 为核心逻辑，建议补表驱动用例（Deployment / ConfigMap / CRD / 删空场景） |
| 4 | 列表接口不回显开关 | 待确认 | `ListReleaseV1/V2` 返回 `helmmanager.Release`，未加字段；详情接口已可回显。若列表页需展示需另加 |
| 5 | Rollback 行为 | 知悉 | `helm rollback` 恢复历史 manifest，回滚到「当时注入过」的版本时注解会回来，属正确语义 |
| 6 | Addons（集群组件） | 知悉 | 复用同一 Action 但不传开关 → 默认 `false` → 行为不回归 |

---

## 六、部署后验收步骤

```javascript
// ① 落库：部署前基线为 0
db.bcshelmmanager_release.countDocuments({skipPaasAnnotations:true})
db.bcshelmmanager_release.countDocuments({skipPaasAnnotations:{$exists:false}})   // 存量 314

// ② 勾选一次后
db.bcshelmmanager_release.countDocuments({skipPaasAnnotations:true})               // 期望 1
db.bcshelmmanager_release.findOne(
  {clusterID:"<集群ID>", namespace:"<ns>", name:"<release名>"},
  {name:1, updateBy:1, updateTime:1, skipPaasAnnotations:1, status:1}
)
```

```bash
# ③ 集群侧（最关键，落库成功 ≠ 注解已删除）
helm get manifest <release名> -n <ns> | grep -c "io.tencent.paas.creator"   # 期望 0
kubectl get deploy <资源名> -n <ns> -o jsonpath='{.metadata.annotations}'   # 期望无 paas 系列
```

> 注意：开关在 `saveDB()` 阶段（Dispatch 之前）即写入 Mongo，此时 helm 可能尚未执行完。因此 Mongo 查到 `true` **不代表注解已删除**，必须执行第 ③ 步。

建议先在测试集群用非核心 release 验证，确认不注入不会引发依赖 paas 注解的组件异常。

---

## 七、工作日报（2026-10-10）

### 今日完成

1. **协议层**：`bcs-helm-manager.proto` 新增 `skipPaasAnnotations` 字段 3 处（`InstallReleaseV1Req` / `UpgradeReleaseV1Req` / `ReleaseDetail`），并完成 protoc 重新生成（pb.go、validate、swagger）及 `bcs-common` 共享包同步。
2. **数据层**：`entity.Release` 新增持久化字段与 `FieldKey` 常量，`Transfer2DetailProto` 支持详情回显。
3. **安装链路**：`installv1.go` → `operation/actions/install.go`，Option 传参、落库、`Validate`(dry-run) 与 `Execute` 条件注入。
4. **更新链路**：`upgradev1.go` → `operation/actions/upgrade.go`，实现 `*bool` 指针回填（未传时继承旧值），保证「可取消勾选」。
5. **注解删除**：`sdk/patch.go` 新增 `removePaasMetadata` 系列函数，清理 `metadata` 与 `spec.template` 下 `annotations` + `labels` 四处路径。
6. **预览一致性**：`preview.go` 从 DB 读取持久化开关值，避免「预览有注解、实际没有」。
7. **排查与修复**：定位并解决 protoc 生成时 `third_party` 挂载源错误（`proto/third_party` 为空目录，正确为 `bcs-helm-manager/third_party`）；解决 Git Bash MSYS 路径转换导致 `docker run -v` 参数被改写的問題。
8. **代码 review**：回滚 Dockerfile 的环境适配改动（`GOPROXY` / `GOSUMDB` / 阿里云源），确认插件版本未变、产物不受影响。

### 明日计划

1. 补 `removePaasMetadata` 单元测试（表驱动，覆盖 Deployment / ConfigMap / CRD / 删空场景）。
2. 与前端确认勾选框改造进度与参数名，完成联调。
3. 跑完整 `go build ./...` 及本地冒烟。
4. 处理 `docs/` 目录（提交或删除）。
5. 与产品确认列表接口是否需要回显开关。

### 风险与阻塞

- **阻塞项**：前端「更新页」勾选框未就绪，后端功能无法端到端验证。
- **兼容性**：已通过反向命名 + `optional` 保证老客户端行为不变；存量 314 条数据无该字段，读取为 `false`（注入），不回归。
- **约束**：`protoc-gen-go` 必须保持 v1.3.2，升级会导致 `*bool` 退化为 `bool`，指针回填逻辑失效。
