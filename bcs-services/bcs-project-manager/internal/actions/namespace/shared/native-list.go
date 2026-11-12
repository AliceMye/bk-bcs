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
	"encoding/json"
	"strings"
	"time"

	spb "google.golang.org/protobuf/types/known/structpb"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/actions/namespace/common"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/common/constant"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/component/clientset"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/config"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/logging"
	nsm "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/store/namespace"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/util/errorx"
	nsutils "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/util/namespace"
	quotautils "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/util/quota"
	"github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/internal/util/stringx"
	proto "github.com/Tencent/bk-bcs/bcs-services/bcs-project-manager/proto/bcsproject"
)

// ListNativeNamespaces implement for ListNativeNamespaces interface
// 共享管理页集群视角调用（projectIDOrCode = '-'）时返回集群全量命名空间，
// 包含未分配（无归属 annotation）的命名空间，并添加ns(配额已用量/来源/可操作状态)字段
func (a *SharedNamespaceAction) ListNativeNamespaces(ctx context.Context,
	req *proto.ListNativeNamespacesRequest, resp *proto.ListNativeNamespacesResponse) error {
	client, err := clientset.GetClientGroup().Client(req.GetClusterID())
	if err != nil {
		logging.Error("get clientset for cluster %s failed, err: %s", req.GetClusterID(), err.Error())
		return err
	}
	nsList, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return errorx.NewClusterErr(err.Error())
	}
	namespaces := nsList.Items
	if req.GetProjectIDOrCode() != "-" {
		project, err := a.model.GetProject(context.TODO(), req.GetProjectIDOrCode())
		if err != nil {
			logging.Error("get project from db failed, err: %s", err.Error())
			return errorx.NewDBErr(err.Error())
		}
		namespaces = nsutils.FilterNamespaces(nsList, true, project.ProjectCode)
	}
	// 批量获取集群内 ResourceQuota：名字与命名空间同名的视为平台分配的默认配额 其余（用户自建）不作为主配额返回
	quotaMap := map[string]v1.ResourceQuota{}
	if quotaList, e := client.CoreV1().ResourceQuotas("").List(ctx, metav1.ListOptions{}); e == nil {
		for _, quota := range quotaList.Items {
			if quota.GetNamespace() == quota.GetName() {
				quotaMap[quota.GetNamespace()] = quota
			}
		}
	} else {
		logging.Error("list resourcequotas in cluster %s failed, err: %s", req.GetClusterID(), e.Error())
	}
	// 批量获取 DB 归属记录（来源/更新时间/审批信息）
	dbRecords := map[string]nsm.Namespace{}
	if dbList, e := a.model.ListNamespacesByCluster(ctx, req.GetClusterID()); e == nil {
		for _, record := range dbList {
			dbRecords[record.Name] = record
		}
	} else {
		logging.Error("list namespace records of cluster %s from db failed, err: %s",
			req.GetClusterID(), e.Error())
	}
	retDatas := []*proto.NativeNamespaceData{}
	for _, namespace := range namespaces {
		retData := &proto.NativeNamespaceData{
			Uid:        string(namespace.GetUID()),
			Name:       namespace.GetName(),
			Status:     string(namespace.Status.Phase),
			CreateTime: namespace.GetCreationTimestamp().UTC().Format(time.RFC3339),
		}
		// 归属项目：未分配（无归属 annotation）的命名空间留空，不跳过
		if projectCode, ok := namespace.Annotations[config.GlobalConf.SharedClusterConfig.AnnoKeyProjCode]; ok {
			if p, err := a.model.GetProject(ctx, projectCode); err == nil {
				retData.ProjectID = p.ProjectID
				retData.ProjectCode = p.ProjectCode
			} else {
				logging.Error("get project %s from db failed, err: %s", projectCode, err.Error())
			}
		}
		// 配额与已用量：只取与命名空间同名的 ResourceQuota，没有则留空
		if quota, ok := quotaMap[namespace.GetName()]; ok {
			retData.Quota, retData.Used, _, _ = quotautils.TransferToProto(&quota)
		}

		record, ok := dbRecords[namespace.GetName()]
		if ok {
			retData.Source = record.Source
			retData.UpdateTime = record.UpdateTime
			retData.ItsmTicketType = record.ItsmTicketType
			retData.ItsmTicketStatus = record.ItsmTicketStatus
		}
		// 可操作判定：vcluster占用、系统命名空间、审批中的命名空间不可操作
		if _, isVcluster := namespace.Annotations[constant.AnnotationKeyVcluster]; isVcluster {
			retData.Allocatable = false
			retData.DisableReason = "vcluster 占用的命名空间，不可分配"
		} else if stringx.StringInSlice(namespace.GetName(), config.GlobalConf.SystemConfig.SystemNameSpaces) {
			retData.Allocatable = false
			retData.DisableReason = "系统命名空间，不可分配"
		} else if ok && record.ItsmTicketStatus == nsm.ItsmTicketStatusCreated {
			retData.Allocatable = false
			retData.DisableReason = "命名空间审批中，暂不可操作"
		} else {
			retData.Allocatable = true
		}
		retDatas = append(retDatas, retData)
	}
	resp.Data = retDatas
	return nil
}

// ListNativeNamespacesContent implement for ListNativeNamespacesContent interface
func (a *SharedNamespaceAction) ListNativeNamespacesContent(ctx context.Context,
	req *proto.ListNativeNamespacesContentRequest, resp *spb.Struct) error {
	client, err := clientset.GetClientGroup().Client(req.GetClusterID())
	if err != nil {
		logging.Error("get clientset for cluster %s failed, err: %s", req.GetClusterID(), err.Error())
		return err
	}
	accept := common.GetAcceptType(ctx)
	nsList, err := client.CoreV1().RESTClient().Get().Resource("namespaces").SetHeader("Accept", accept).DoRaw(ctx)
	if err != nil {
		return errorx.NewClusterErr(err.Error())
	}

	if req.GetProjectIDOrCode() == "-" {
		return resp.UnmarshalJSON(nsList)
	}

	project, errr := a.model.GetProject(context.TODO(), req.GetProjectIDOrCode())
	if errr != nil {
		logging.Error("get project from db failed, err: %s", errr.Error())
		return errorx.NewDBErr(errr.Error())
	}
	// Table和json返回的结果不同
	if strings.Contains(accept, "Table") {
		mt := &metav1.Table{}
		err = json.Unmarshal(nsList, mt)
		if err != nil {
			return errorx.NewInnerErr(err.Error())
		}
		mt, err = nsutils.FilterTableNamespaces(mt, project.ProjectCode)
		if err != nil {
			return errorx.NewInnerErr(err.Error())
		}
		nsList, err = json.Marshal(mt)
		if err != nil {
			return errorx.NewInnerErr(err.Error())
		}

		return resp.UnmarshalJSON(nsList)
	}

	mt := &v1.NamespaceList{}
	err = json.Unmarshal(nsList, mt)
	if err != nil {
		return errorx.NewInnerErr(err.Error())
	}
	mt.Items = nsutils.FilterNamespaces(mt, true, project.ProjectCode)
	nsList, err = json.Marshal(mt)
	if err != nil {
		return errorx.NewInnerErr(err.Error())
	}
	return resp.UnmarshalJSON(nsList)
}
