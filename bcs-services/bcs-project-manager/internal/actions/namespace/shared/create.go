/*
 * Tencent is pleased to support the open source community by making Blueking Container Service available.
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package shared

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Tencent/bk-bcs/bcs-services/pkg/bcs-auth/middleware"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/common/constant"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/common/envs"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/component/bcscc"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/component/clientset"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/component/clustermanager"
	v2 "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/component/itsm/v2"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/config"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/logging"
	nsm "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/store/namespace"
	vdm "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/store/variabledefinition"
	vvm "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/store/variablevalue"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/util/errorx"
	quotautils "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/util/quota"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/util/stringx"
	proto "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/proto/bcsproject"
)

// NamespacePrefix namespace in shared cluster must be prefixed by it
var NamespacePrefix = envs.BCSNamespacePrefix + "-%s-"

// CreateNamespace implement for CreateNamespace interface
func (a *SharedNamespaceAction) CreateNamespace(ctx context.Context,
	req *proto.CreateNamespaceRequest, resp *proto.CreateNamespaceResponse) error {
	if err := a.validateCreate(ctx, req); err != nil {
		return err
	}
	var username string
	if authUser, gErr := middleware.GetUserFromContext(ctx); gErr == nil {
		username = authUser.GetUsername()
	}
	// ITSM 开启时，仅非集群所属项目管理员走审批；所属项目管理员为可见范围内项目创建无需审批
	if config.GlobalConf.ITSM.Enable && !a.isClusterOwnerManager(ctx, req.GetClusterID(), username) {
		return a.submitCreateTicket(ctx, req, username)
	}
	// 直接创建ns，不自动授予创建者命名空间权限
	if err := a.createNamespaceInCluster(ctx, req, username); err != nil {
		return err
	}
	nsModel := &nsm.Namespace{
		ProjectCode: req.GetProjectCode(),
		ClusterID:   req.GetClusterID(),
		Name:        req.GetName(),
		Creator:     username,
		Updater:     username,
		ResourceQuota: &nsm.Quota{
			CPURequests:    req.GetQuota().GetCpuRequests(),
			CPULimits:      req.GetQuota().GetCpuLimits(),
			MemoryRequests: req.GetQuota().GetMemoryRequests(),
			MemoryLimits:   req.GetQuota().GetMemoryLimits(),
		},
		UpdateTime: time.Now().UTC().Format(time.RFC3339),
	}
	if err := a.model.CreateNamespace(ctx, nsModel); err != nil {
		logging.Error("create namespace %s/%s in db failed, err: %s",
			req.GetClusterID(), req.GetName(), err.Error())
		return errorx.NewDBErr(err.Error())
	}
	// 变量值
	for _, variable := range req.GetVariables() {
		entity := &vvm.VariableValue{
			VariableID: variable.GetId(),
			Value:      variable.GetValue(),
			Scope:      vdm.VariableScopeNamespace,
			ClusterID:  req.GetClusterID(),
			Namespace:  req.GetName(),
		}
		if err := a.model.UpsertVariableValue(ctx, entity); err != nil {
			return err
		}
	}
	go func() {
		if err := bcscc.CreateNamespace(req.GetProjectCode(), req.GetClusterID(), req.GetName(), username); err != nil {
			logging.Error("[ALARM-CC-NAMESPACE] create namespace %s/%s/%s in paas-cc failed, err: %s",
				req.GetProjectCode(), req.GetClusterID(), req.GetName(), err.Error())
		}
	}()
	return nil
}

func (a *SharedNamespaceAction) validateCreate(ctx context.Context, req *proto.CreateNamespaceRequest) error {
	// 归属项目必须在集群可见范围内（集群所属项目自身可直接创建）
	cluster, err := clustermanager.GetCluster(ctx, req.ClusterID, true)
	if err != nil {
		logging.Error("get cluster %s failed, err: %s", req.ClusterID, err.Error())
		return errorx.NewClusterErr(err.Error())
	}
	ownerProject, err := a.model.GetProject(ctx, cluster.GetProjectID())
	if err != nil {
		logging.Error("get cluster owner project %s failed, err: %s", cluster.GetProjectID(), err.Error())
		return errorx.NewDBErr(err.Error())
	}
	if req.ProjectCode != ownerProject.ProjectCode {
		targetProject, tErr := a.model.GetProject(ctx, req.ProjectCode)
		if tErr != nil {
			return errorx.NewReadableErr(errorx.ProjectNotExistsErr, "归属项目不存在")
		}
		if err := a.checkProjectInSharedRanges(cluster, targetProject); err != nil {
			return err
		}
	}
	// check is namespace name valid
	fullPrefix := fmt.Sprintf(NamespacePrefix, req.ProjectCode)
	platformPrefix := envs.BCSNamespacePrefix + "-"
	if !strings.HasPrefix(req.Name, fullPrefix) {
		customPart := strings.TrimPrefix(req.Name, platformPrefix)
		wouldExceed := len(fullPrefix)+len(customPart) > 63
		if !(strings.HasPrefix(req.Name, platformPrefix) && wouldExceed) {
			return errorx.NewReadableErr(errorx.ParamErr, fmt.Sprintf("共享集群命名空间必须以 %s-[projectCode]- 开头",
				envs.BCSNamespacePrefix))
		}
	}
	// check is namespace name exists
	stagings, _ := a.model.ListNamespacesByItsmTicketType(ctx,
		req.ProjectCode, req.ClusterID, []string{nsm.ItsmTicketTypeCreate})
	for _, staging := range stagings {
		if staging.Name == req.Name {
			return errorx.NewReadableErr(errorx.ParamErr, fmt.Sprintf("命名空间 [%s] 已存在", req.Name))
		}
	}
	client, err := clientset.GetClientGroup().Client(req.ClusterID)
	if err != nil {
		logging.Error("get clientset for cluster %s failed, err: %s", req.ClusterID, err.Error())
		return err
	}
	_, err = client.CoreV1().Namespaces().Get(ctx, req.Name, metav1.GetOptions{})
	if err == nil {
		return errorx.NewReadableErr(errorx.ParamErr, fmt.Sprintf("命名空间 [%s] 已存在", req.Name))
	}
	if !errors.IsNotFound(err) {
		logging.Error("get namespace in cluster %s failed, err: %s", req.ClusterID, err.Error())
		return errorx.NewClusterErr(err.Error())
	}
	// 创建/分配时强制设置 CPU/内存配额，共享集群创建不允许缺省配额
	if req.GetQuota() == nil {
		return errorx.NewReadableErr(errorx.ParamErr, "命名空间配额不能为空")
	}
	// check resourceQuota validate
	if err := quotautils.ValidateResourceQuota(req.Quota); err != nil {
		return err
	}
	if !config.GlobalConf.SharedClusterConfig.AllowAdjustQuota {
		if err := quotautils.ValidateQuotaEquality(req.Quota); err != nil {
			return err
		}
	}
	return nil
}

// isClusterOwnerManager 判断用户是否为集群所属项目的管理员
func (a *SharedNamespaceAction) isClusterOwnerManager(ctx context.Context, clusterID, username string) bool {
	if username == "" {
		return false
	}
	cluster, err := clustermanager.GetCluster(ctx, clusterID, true)
	if err != nil {
		logging.Error("get cluster %s from cluster-manager failed, err: %s", clusterID, err.Error())
		return false
	}
	ownerProject, err := a.model.GetProject(ctx, cluster.GetProjectID())
	if err != nil {
		logging.Error("get cluster owner project %s failed, err: %s", cluster.GetProjectID(), err.Error())
		return false
	}
	for _, manager := range stringx.SplitString(ownerProject.Managers) {
		if manager == username {
			return true
		}
	}
	return false
}

// submitCreateTicket 提交创建命名空间审批单，审批通过后由回调创建
func (a *SharedNamespaceAction) submitCreateTicket(ctx context.Context,
	req *proto.CreateNamespaceRequest, username string) error {
	namespace := &nsm.Namespace{
		ProjectCode: req.GetProjectCode(),
		ClusterID:   req.GetClusterID(),
		Name:        req.GetName(),
		Creator:     username,
		Managers:    username,
		ResourceQuota: &nsm.Quota{
			CPURequests:    req.GetQuota().GetCpuRequests(),
			CPULimits:      req.GetQuota().GetCpuLimits(),
			MemoryRequests: req.GetQuota().GetMemoryRequests(),
			MemoryLimits:   req.GetQuota().GetMemoryLimits(),
		},
		UpdateTime: time.Now().UTC().Format(time.RFC3339),
	}
	variables := []*nsm.Variable{}
	for _, variable := range req.GetVariables() {
		variables = append(variables, &nsm.Variable{
			VariableID: variable.Id,
			ClusterID:  variable.ClusterID,
			Namespace:  variable.Namespace,
			Key:        variable.Key,
			Value:      variable.Value,
		})
	}
	namespace.Variables = variables
	cpuLimits, err := resource.ParseQuantity(req.GetQuota().GetCpuLimits())
	if err != nil {
		logging.Error("parse quantity cpu limits failed, err: %s", err.Error())
		return err
	}
	memoryLimits, err := resource.ParseQuantity(req.GetQuota().GetMemoryLimits())
	if err != nil {
		logging.Error("parse quantity memory limits failed, err: %s", err.Error())
		return err
	}
	// memoryLimits.Value() return unit is byte， needs to be converted to Gi (divide 2^30)
	itsmResp, err := v2.SubmitCreateNamespaceTicket(ctx, username, req.GetProjectCode(), req.GetClusterID(), req.GetName(),
		int(cpuLimits.Value()), int(memoryLimits.Value()/int64(math.Pow(2, 30))))
	if err != nil {
		logging.Error("itsm create ticket failed, err: %s", err.Error())
		return err
	}
	namespace.ItsmTicketType = nsm.ItsmTicketTypeCreate
	namespace.ItsmTicketURL = itsmResp.TicketURL
	namespace.ItsmTicketStatus = nsm.ItsmTicketStatusCreated
	namespace.ItsmTicketSN = itsmResp.SN
	if err := a.model.CreateNamespace(ctx, namespace); err != nil {
		logging.Error("create namespace %s/%s in db failed, err: %s", req.GetClusterID(), req.GetName(), err.Error())
		return err
	}
	return nil
}

// createNamespaceInCluster 在集群中直接创建命名空间并下发配额（不做IAM授权）
func (a *SharedNamespaceAction) createNamespaceInCluster(ctx context.Context,
	req *proto.CreateNamespaceRequest, creator string) error {
	client, err := clientset.GetClientGroup().Client(req.GetClusterID())
	if err != nil {
		return err
	}
	ns := &corev1.Namespace{}
	ns.SetName(req.GetName())
	ns.SetAnnotations(map[string]string{
		config.GlobalConf.SharedClusterConfig.AnnoKeyProjCode: req.GetProjectCode(),
		constant.AnnotationKeyCreator:                         creator,
	})
	if _, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		logging.Error("create namespace %s in cluster %s failed, err: %s",
			req.GetName(), req.GetClusterID(), err.Error())
		return errorx.NewClusterErr(err.Error())
	}
	quota := &corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{}},
	}
	quota.SetName(req.GetName())
	quota.SetNamespace(req.GetName())
	if err := quotautils.LoadFromModel(quota, &nsm.Quota{
		CPURequests:    req.GetQuota().GetCpuRequests(),
		CPULimits:      req.GetQuota().GetCpuLimits(),
		MemoryRequests: req.GetQuota().GetMemoryRequests(),
		MemoryLimits:   req.GetQuota().GetMemoryLimits(),
	}); err != nil {
		return err
	}
	if _, err := client.CoreV1().ResourceQuotas(req.GetName()).Create(ctx, quota, metav1.CreateOptions{}); err != nil {
		logging.Error("create resourcequota %s in cluster %s failed, err: %s",
			req.GetName(), req.GetClusterID(), err.Error())
		return errorx.NewClusterErr(err.Error())
	}
	return nil
}
