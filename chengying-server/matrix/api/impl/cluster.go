// Licensed to Apache Software Foundation(ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation(ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package impl

import (
	"bytes"
	sysContext "context"
	"database/sql"
	"dtstack.com/dtstack/easymatrix/matrix/model/upgrade"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dtstack.com/dtstack/easymatrix/addons/easykube/pkg/view/response"
	"dtstack.com/dtstack/easymatrix/addons/easymonitor/pkg/monitor/events"
	oldclient "dtstack.com/dtstack/easymatrix/addons/oldkube/pkg/client-go"
	apibase "dtstack.com/dtstack/easymatrix/go-common/api-base"
	dbhelper "dtstack.com/dtstack/easymatrix/go-common/db-helper"
	"dtstack.com/dtstack/easymatrix/matrix/agent"
	"dtstack.com/dtstack/easymatrix/matrix/api/k8s/view"
	"dtstack.com/dtstack/easymatrix/matrix/base"
	"dtstack.com/dtstack/easymatrix/matrix/encrypt/aes"
	"dtstack.com/dtstack/easymatrix/matrix/enums"
	"dtstack.com/dtstack/easymatrix/matrix/event"
	"dtstack.com/dtstack/easymatrix/matrix/grafana"
	"dtstack.com/dtstack/easymatrix/matrix/host"
	clustergenerator "dtstack.com/dtstack/easymatrix/matrix/k8s/cluster"
	"dtstack.com/dtstack/easymatrix/matrix/k8s/constant"
	kdeploy "dtstack.com/dtstack/easymatrix/matrix/k8s/deploy"
	"dtstack.com/dtstack/easymatrix/matrix/k8s/docker"
	"dtstack.com/dtstack/easymatrix/matrix/k8s/kube"
	kmodel "dtstack.com/dtstack/easymatrix/matrix/k8s/model"
	"dtstack.com/dtstack/easymatrix/matrix/k8s/monitor"
	"dtstack.com/dtstack/easymatrix/matrix/k8s/resource"
	"dtstack.com/dtstack/easymatrix/matrix/k8s/resource/endpoints"
	"dtstack.com/dtstack/easymatrix/matrix/k8s/resource/service"
	kutil "dtstack.com/dtstack/easymatrix/matrix/k8s/util"
	ksocket "dtstack.com/dtstack/easymatrix/matrix/k8s/web-socket"
	xke_service "dtstack.com/dtstack/easymatrix/matrix/k8s/xke-service"
	"dtstack.com/dtstack/easymatrix/matrix/log"
	"dtstack.com/dtstack/easymatrix/matrix/model"
	modelkube "dtstack.com/dtstack/easymatrix/matrix/model/kube"
	"dtstack.com/dtstack/easymatrix/matrix/util"
	"dtstack.com/dtstack/easymatrix/schema"
	errors2 "github.com/juju/errors"
	"github.com/kataras/iris/context"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	IMAGE_DIR    = "dtstack-runtime/images"
	IMAGE_SUFFIX = ".tar"
)

func PushOperatorImage(registry string) {
	//推送operator相关的镜像
	log.Infof("prepare push images to registry %v", registry)
	ImageDir := filepath.Join(base.WebRoot, IMAGE_DIR)
	err := filepath.Walk(ImageDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ImageDir == path {
			return nil
		}
		if info.IsDir() {
			return nil
		}

		if !strings.HasSuffix(path, IMAGE_SUFFIX) {
			log.Infof("not found image file in: %v", ImageDir)
			return nil
		}

		log.Infof("find image file: %v", path)
		log.Infof("docker load images file %v", path)
		LoadImage := fmt.Sprintf("docker load -i %s | grep 'Loaded image'|awk '{print $3}'", path)
		log.Debugf("exec docker load command: %v", LoadImage)
		cmd := exec.Command("/bin/sh", "-c", LoadImage)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			log.Errorf("docker load image %v error: %v", path, err)
			return err
		}
		//获取镜像原始标签同时去除换行符
		sourceTag := strings.Replace(out.String(), "\n", "", -1)
		newTag := registry + "/" + sourceTag
		log.Infof("docker load images %v file success, source tag is: %v", path, sourceTag)

		//镜像打标签
		err = docker.Tag(newTag, sourceTag)
		if err != nil {
			log.Errorf("docker tag error:", err)
			return err
		}
		log.Infof("tag image %v success", newTag)
		//推送镜像
		push := exec.Command("docker", "push", newTag)
		if err := push.Run(); err != nil {
			log.Errorf("push image %v error", err)
			return err
		}
		log.Infof("push image %v success", path)

		return nil
	})

	if err != nil {
		log.Errorf("push image error:", err.Error())
		return
	}
}

// k8s-镜像仓库crud
func CreateImageStore(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->CreateImageStore] CreateImageStore from EasyMatrix API ")

	param := model.ImageStore{}
	if err := ctx.ReadJSON(&param); err != nil {
		return fmt.Errorf("ReadJSON err: %v", err)
	}
	//prevent the possibility of not being allowed to login the image store,but the current process must have a default image strore
	if param.Name != "skip" {
		err := docker.Login(param.Username, param.Address, param.Password)
		if err != nil {
			return fmt.Errorf("image store login fail")
		}
	}
	id, err := model.ClusterImageStore.InsertImageStore(param)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	go PushOperatorImage(param.Address)
	return map[string]interface{}{
		"id":        id,
		"clusterId": param.ClusterId,
		"name":      param.Name,
		"alias":     param.Alias,
		"address":   param.Address,
		"username":  param.Username,
		"password":  param.Password,
		"email":     param.Email,
	}

}

func DeleteImageStore(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->DeleteImageStore] DeleteImageStore from EasyMatrix API ")

	param := struct {
		Id []int `json:"id"`
	}{}
	if err := ctx.ReadJSON(&param); err != nil {
		return fmt.Errorf("ReadJSON err: %v", err)
	}

	for _, v := range param.Id {
		err := model.ClusterImageStore.DeleteImageStoreById(v)
		if err != nil {
			return fmt.Errorf("Database err: %v", err)
		}
	}
	return nil
}

func UpdateImageStore(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->UpdateImageStore] UpdateImageStore from EasyMatrix API ")
	param := model.ImageStore{}
	if err := ctx.ReadJSON(&param); err != nil {
		return fmt.Errorf("ReadJSON err: %v", err)
	}
	if param.Name != "skip" {
		err := docker.Login(param.Username, param.Address, param.Password)
		if err != nil {
			return fmt.Errorf("image store login fail")
		}
	}
	err := model.ClusterImageStore.UpdateImageStoreById(param)
	if err != nil {
		return err
	}
	store, err := model.ClusterImageStore.GetImageStoreInfoById(param.Id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	return map[string]interface{}{
		"id":         store.Id,
		"clusterId":  store.ClusterId,
		"name":       store.Name,
		"alias":      store.Alias,
		"address":    store.Address,
		"username":   store.Username,
		"password":   store.Password,
		"email":      store.Email,
		"is_default": store.IsDefault,
	}
}

func GetImageStoreInfoByClusterId(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->GetImageStoreInfoByClusterId] GetImageStoreInfoByClusterId from EasyMatrix API ")
	clusterId, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	stores, err := model.ClusterImageStore.GetImageStoreInfoByClusterId(clusterId)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	list := []map[string]interface{}{}
	for _, store := range stores {
		list = append(list, map[string]interface{}{
			"id":         store.Id,
			"clusterId":  store.ClusterId,
			"name":       store.Name,
			"alias":      store.Alias,
			"address":    store.Address,
			"username":   store.Username,
			"password":   store.Password,
			"email":      store.Email,
			"is_default": store.IsDefault,
		})
	}
	return map[string]interface{}{
		"count": len(stores),
		"list":  list,
	}
}

func GetImageStoreInfoById(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->GetImageStoreInfoById] GetImageStoreInfoById from EasyMatrix API ")
	id, err := ctx.Params().GetInt("store_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	store, err := model.ClusterImageStore.GetImageStoreInfoById(id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	return map[string]interface{}{
		"id":         store.Id,
		"clusterId":  store.ClusterId,
		"name":       store.Name,
		"alias":      store.Alias,
		"address":    store.Address,
		"username":   store.Username,
		"password":   store.Password,
		"email":      store.Email,
		"is_default": store.IsDefault,
	}
}

func CheckDefaultImageStore(ctx context.Context) apibase.Result {
	log.Debugf("CheckDefaultImageStore: %v", ctx.Request().RequestURI)

	clusterId, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	store, err := model.ClusterImageStore.GetDefaultStoreByClusterId(clusterId)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("database err:%v", err)
	}
	exist := store.Id > 0
	return map[string]bool{
		"exist": exist,
	}
}

func SetDefaultImageStore(ctx context.Context) apibase.Result {
	log.Debugf("SetDefaultImageStore: %v", ctx.Request().RequestURI)
	param := make(map[string]int)
	err := ctx.ReadJSON(&param)
	if err != nil {
		return fmt.Errorf("read json err:%v", err)
	}
	err = model.ClusterImageStore.SetDefaultById(param["id"], param["clusterId"])
	if err != nil {
		return fmt.Errorf("database err:%v", err)
	}
	return map[string]string{
		"message": "success",
	}
}

// 主机集群crud
func CreateHostCluster(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->CreateHostCluster] CreateHostCluster from EasyMatrix API ")

	param := model.ClusterInfo{}
	if err := ctx.ReadJSON(&param); err != nil {
		return fmt.Errorf("ReadJSON err: %v", err)
	}
	userId, err := ctx.Values().GetInt("userId")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	userName := ctx.Values().GetString("username")
	id, err := model.DeployClusterList.InsertHostCluster(param, userName)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	defer func() {
		if err := addSafetyAuditRecord(ctx, "集群管理", "创建集群", "集群名称："+param.Name); err != nil {
			log.Errorf("failed to add safety audit record\n")
		}
	}()
	// 集群创建的时候为该集群初始化角色
	err = model.HostRole.InitNewCluster(id)
	if err != nil {
		return fmt.Errorf("host role init err: %v", err)
	}

	err, userInfo := model.UserList.GetInfoByUserId(userId)
	if err != nil {
		log.Errorf("GetInfoByUserId %v", err)
		return err
	}
	//写入权限
	if userInfo.RoleId != model.ROLE_ADMIN_ID {
		err, _ := model.ClusterRightList.InsertUserClusterRight(userId, id)
		if err != nil {
			log.Errorf(err.Error())
			return fmt.Errorf("can not insert ClusterRight, err : %v", err.Error())
		}
	}

	return map[string]interface{}{
		"id":   id,
		"name": param.Name,
	}
}

func DeleteHostCluster(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->DeleteHostCluster] DeleteHostCluster from EasyMatrix API ")
	id, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	info, err := model.DeployClusterList.GetClusterInfoById(id)
	if err != nil {
		log.Errorf("%v", err)
	}
	err = model.DeployClusterList.DeleteHostClusterById(id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	err = model.ClusterRightList.DeleteByClusterId(id)
	if !errors.Is(err, sql.ErrNoRows) && err != nil {
		return fmt.Errorf("Database err: %v", err)
	}

	defer func() {
		if err := addSafetyAuditRecord(ctx, "集群管理", "删除集群", "集群名称："+info.Name); err != nil {
			log.Errorf("failed to add safety audit record\n")
		}
	}()
	return nil
}

func UpdateHostCluster(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->UpdateCluster] UpdateCluster from EasyMatrix API ")
	param := model.ClusterInfo{}
	if err := ctx.ReadJSON(&param); err != nil {
		return fmt.Errorf("ReadJSON err: %v", err)
	}
	userName := ctx.Values().GetString("username")
	err := model.DeployClusterList.UpdateHostCluster(param, userName)
	if err != nil {
		return err
	}
	cluster, err := model.DeployClusterList.GetClusterInfoById(param.Id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	defer func() {
		if err := addSafetyAuditRecord(ctx, "集群管理", "编辑集群", "集群名称："+cluster.Name); err != nil {
			log.Errorf("failed to add safety audit record\n")
		}
	}()
	return map[string]interface{}{
		"id":   cluster.Id,
		"name": cluster.Name,
	}
}

func GetHostClusterInfo(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->GetHostClusterInfo] GetHostClusterInfo from EasyMatrix API ")
	id, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	cluster, err := model.DeployClusterList.GetClusterInfoById(id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	return map[string]interface{}{
		"id":   cluster.Id,
		"name": cluster.Name,
		"desc": cluster.Desc,
		"tags": cluster.Tags,
	}
}
func EditRole(ctx context.Context) apibase.Result {
	log.Debugf("EditRole: %v", ctx.Request().RequestURI)
	var reqParams []struct {
		Sid        string `json:"sid"`
		RoleIdList []int  `json:"role_id_list"`
	}
	err := ctx.ReadJSON(&reqParams)
	if err != nil {
		log.Errorf("入参reqParams解析失败: %v", err)
		return err
	}
	for _, reqParam := range reqParams {
		sort.Ints(reqParam.RoleIdList)
		//roleListStr 格式为  1,2,3
		roleListStr := strings.Replace(strings.Trim(fmt.Sprint(reqParam.RoleIdList), "[]"), " ", ",", -1)
		err = model.DeployHostList.UpdateRoleBySid(reqParam.Sid, roleListStr)
		if err != nil {
			return err
		}
	}
	return nil
}

func RoleList(ctx context.Context) apibase.Result {
	log.Debugf("RoleList: %v", ctx.Request().RequestURI)
	id, err := ctx.URLParamInt("cluster_id")
	if err != nil {
		return err
	}
	roleList, err := model.HostRole.GetRoleListByClusterId(id)
	if err != nil {
		return err
	}
	return roleList
}

func RoleRename(ctx context.Context) apibase.Result {
	log.Debugf("RoleRename: %v", ctx.Request().RequestURI)
	var reqParams struct {
		ClusterId   int    `json:"cluster_id"`
		RoleId      int    `json:"role_id"`
		NewRoleName string `json:"new_name"`
	}
	err := ctx.ReadJSON(&reqParams)
	if err != nil {
		log.Errorf("入参reqParams解析失败: %v", err)
		return err
	}
	roleInfo, err := model.HostRole.GetRoleInfoById(reqParams.RoleId)
	if err != nil {
		return fmt.Errorf("未查询到该角色")
	}
	if roleInfo.RoleType == model.DefaultRoleType {
		return fmt.Errorf("默认角色不支持重命名")
	}

	info, err := model.HostRole.GetRoleInfoByRoleNameAndClusterId(reqParams.ClusterId, reqParams.NewRoleName)

	//如果查询到的话
	if err == nil {
		//如果新就角色名字一样  则不做任何修改
		if info.Id == reqParams.RoleId {
			return nil
		}
		return fmt.Errorf("该角色已经存在")
	}

	//数据库异常
	if err != sql.ErrNoRows {
		return err
	}

	return model.HostRole.ReNameByRoleId(reqParams.RoleId, reqParams.NewRoleName)
}

func RoleInfo(ctx context.Context) apibase.Result {
	log.Debugf("RoleInfo: %v", ctx.Request().RequestURI)
	clusterId, err := ctx.URLParamInt("cluster_id")
	if err != nil {
		return err
	}
	hostInfos, err := model.DeployHostList.GetHostListByClusterId(clusterId)

	type respStruct struct {
		Sid      string               `json:"sid"`
		Ip       string               `json:"ip"`
		RoleInfo []model.HostRoleInfo `json:"role_info"`
	}
	var resp []respStruct
	for _, info := range hostInfos {

		if info.RoleList.Valid && strings.TrimSpace(info.RoleList.String) != "" {
			var roleId []int
			for _, id := range strings.Split(info.RoleList.String, ",") {
				roleIdInt, err := strconv.Atoi(id)
				if err != nil {
					return err
				}
				roleId = append(roleId, roleIdInt)
			}
			roleNameList, err := model.HostRole.GetRoleListByRoleIdList(roleId)
			if err != nil {
				return err
			}
			resp = append(resp, respStruct{
				Sid:      info.SidecarId,
				Ip:       info.Ip,
				RoleInfo: roleNameList,
			})
		}
	}
	if err != nil {
		return err
	}
	return resp
}

//角色删除
func RoleDelete(ctx context.Context) apibase.Result {
	log.Debugf("RoleDelete: %v", ctx.Request().RequestURI)
	var reqParams struct {
		ClusterId int `json:"cluster_id"`
		RoleId    int `json:"role_id"`
	}
	err := ctx.ReadJSON(&reqParams)
	if err != nil {
		log.Errorf("入参reqParams解析失败: %v", err)
		return err
	}
	roleInfo, err := model.HostRole.GetRoleInfoById(reqParams.RoleId)
	if err != nil {
		return err
	}
	if roleInfo.RoleType == model.DefaultRoleType {
		return fmt.Errorf("内置类型无法被删除")
	}
	hosts, err := model.DeployHostList.GetHostListByClusterId(reqParams.ClusterId)
	for _, info := range hosts {
		//要注意 strings.Split("", ",") 的长度为 1
		if info.RoleList.Valid && strings.TrimSpace(info.RoleList.String) != "" {
			strList := strings.Split(info.RoleList.String, ",")
			for idx, ridStr := range strList {
				if ridStr == strconv.Itoa(reqParams.RoleId) {
					strList = append(strList[:idx], strList[idx+1:]...)
				}
			}
			sort.Strings(strList)
			err = model.DeployHostList.UpdateRoleBySid(info.SidecarId, strings.Join(strList, ","))
			if err != nil {
				return err
			}
		}
	}
	return model.HostRole.DeleteRole(reqParams.RoleId)
}

//添加角色
func RoleAdd(ctx context.Context) apibase.Result {
	log.Debugf("RoleAdd: %v", ctx.Request().RequestURI)
	var reqParams struct {
		ClusterId int    `json:"cluster_id"`
		RoleName  string `json:"role_name"`
	}
	err := ctx.ReadJSON(&reqParams)
	if err != nil {
		return err
	}

	_, err = model.HostRole.GetRoleInfoByRoleNameAndClusterId(reqParams.ClusterId, reqParams.RoleName)
	//如果查询到了 证明角色重复
	if err == nil {
		return fmt.Errorf("角色名称重复，请重新输入")
	} else {
		//如果没查询到了并且不是没到错误
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	//正常流程
	err = model.HostRole.AddRole(reqParams.ClusterId, reqParams.RoleName)
	if err != nil {
		return err
	}
	return nil
}

type svcDeployInfo struct {
	Name    string
	SidList []string
}

//自动部署dto
type productDeployInfo struct {
	Pid                int                  //pid
	Name               string               //name
	ServiceSeq         []svcDeployInfo      //角色编排的服务顺序
	UncheckServiceList []string             //未勾选的服务  顺序无要求
	Schema             *schema.SchemaConfig //schema
}

func getSvcSeq(res *[]string, svc string, sc *schema.SchemaConfig, defaultRoleNameMap map[string]struct{}) {
	depSvcListFromDependOn := sc.Service[svc].DependsOn
	var depSvcListFromAffinity []string
	for _, role := range sc.Service[svc].Orchestration.Affinity {
		//如果不是默认的角色 要把它当做服务的依赖  比如 pushgateway 的 Affinity 为 prometheus 那么等同于 pushgateway dependon prometheus
		if _, ok := defaultRoleNameMap[role]; !ok {
			depSvcListFromAffinity = append(depSvcListFromAffinity, role)
		}
	}
	//当两者都为空时意味该服务是根服务
	if len(depSvcListFromDependOn) == 0 && len(depSvcListFromAffinity) == 0 {
		*res = append(*res, svc)
		return
	}

	for _, s := range append(depSvcListFromDependOn, depSvcListFromAffinity...) {
		getSvcSeq(res, s, sc, defaultRoleNameMap)
		*res = append(*res, svc)
	}
}

//获取某个包的必选依赖包
func getProdBaseSet(product productDeployInfo) []string {
	var productList []string
	temp := map[string]struct{}{}
	for svcName, config := range product.Schema.Service {
		if strings.TrimSpace(config.BaseProduct) != "" && config.BaseAtrribute != "optional" {
			if _, ok := temp[svcName]; !ok {
				temp[svcName] = struct{}{}
				productList = append(productList, config.BaseProduct)
			}
		}
	}
	return util.RemoveDuplicateStrElement(productList)
}

func getProdSeq(res *[]productDeployInfo, pInfoMap map[string]productDeployInfo, product productDeployInfo) error {
	prodBaseList := getProdBaseSet(product)
	if len(prodBaseList) == 0 {
		var resSvcDuplicateSeq []string
		defaultRoleNameMap := model.HostRole.GetDefaultRoleNameMap()

		for _, svc := range product.ServiceSeq {
			getSvcSeq(&resSvcDuplicateSeq, svc.Name, product.Schema, defaultRoleNameMap)
		}
		resSvcNameSeq := util.RemoveDuplicateStrElement(resSvcDuplicateSeq)
		var resSvcSeq []svcDeployInfo
		for _, svcName := range resSvcNameSeq {
			resSvcSeq = append(resSvcSeq, svcDeployInfo{
				Name:    svcName,
				SidList: nil,
			})
		}
		*res = append(*res, productDeployInfo{
			Pid:                product.Pid,
			Name:               product.Name,
			UncheckServiceList: product.UncheckServiceList,
			ServiceSeq:         resSvcSeq,
			Schema:             product.Schema,
		})
		return nil
	} else {
		for _, productName := range prodBaseList {
			if info, ok := pInfoMap[productName]; !ok {
				return fmt.Errorf("未解析到必备产品包:%s", productName)
			} else {
				err := getProdSeq(res, pInfoMap, pInfoMap[info.Name])
				if err != nil {
					return err
				}
				*res = append(*res, product)
			}
		}
	}
	return nil
}

type productInfo struct {
	Id                 int      `json:"id"`
	Name               string   `json:"name"`
	ServiceList        []string `json:"service_list"`
	UncheckServiceList []string `json:"uncheck_service_list"`
}

func RemoveDuplicateProdElement(elements []productDeployInfo) []productDeployInfo {
	result := make([]productDeployInfo, 0, len(elements))
	temp := map[string]struct{}{}
	for _, item := range elements {
		if _, ok := temp[item.Name]; !ok {
			temp[item.Name] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}

const DTFrontProductName = "DTFront"

func getProductInfoSeq(pidList []string, pInfoMap map[string]productDeployInfo, productInfo []productInfo) ([]productDeployInfo, error) {
	log.Debugf("getProductInfoSeq  pidList: %v", pidList)
	var productDeployInfos []productDeployInfo
	var hasDTFront bool
	for _, product := range productInfo {
		info, err := model.DeployProductList.GetProductInfoById(product.Id)
		if err != nil {
			return nil, err
		}
		if info.ProductName == DTFrontProductName {
			hasDTFront = true
		}
	}

	//DTFront 特殊处理 有 DTFront 就先部署 DTFront  由于 DTFront 不依赖任何其他包 那么根据本算法先处理 DTFront   DTFront 就会被放到第一位
	if hasDTFront {
		err := getProdSeq(&productDeployInfos, pInfoMap, pInfoMap[DTFrontProductName])
		if err != nil {
			return nil, err
		}
	}
	for _, product := range productInfo {
		err := getProdSeq(&productDeployInfos, pInfoMap, pInfoMap[product.Name])
		if err != nil {
			return nil, err
		}
	}
	resProdSeq := RemoveDuplicateProdElement(productDeployInfos)
	return resProdSeq, nil
}

type HostInfo struct {
	MemSize   int64          `db:"mem_size"`
	MemUsage  int64          `db:"mem_usage"`
	DiskUsage sql.NullString `db:"disk_usage"`
	CpuCores  int            `db:"cpu_cores"`
	CpuUsage  float64        `db:"cpu_usage"`
	Load1     float64        `db:"load1"`
	Status    int            `db:"status"`
	LocalIp   string         `db:"local_ip"`
	Id        int            `db:"id"`
	Updated   base.Time      `db:"updated"`
}

// AutoOrchestration 主机自动编排
func AutoOrchestration(ctx context.Context) apibase.Result {
	log.Debugf("AutoOrchestration: %v", ctx.Request().RequestURI)
	var reqParams struct {
		ClusterId          int           `json:"cluster_id"`
		ProductLineName    string        `json:"product_line_name"`
		ProductLineVersion string        `json:"product_line_version"`
		ProductInfo        []productInfo `json:"product_info"`
	}
	err := ctx.ReadJSON(&reqParams)
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	pInfoMap := map[string]productDeployInfo{}
	var newPidList []string
	for _, product := range reqParams.ProductInfo {
		info, err := model.DeployProductList.GetProductInfoById(product.Id)
		if err != nil {
			log.Errorf("%v", err)
			return err
		}
		var svcDeployInfos []svcDeployInfo
		sc, err := schema.Unmarshal(info.Product)
		if err != nil {
			log.Errorf("%v", err)
			return err
		}
		newPidList = append(newPidList, strconv.Itoa(product.Id))
		for _, name := range product.ServiceList {
			svcDeployInfos = append(svcDeployInfos, svcDeployInfo{
				Name:    name,
				SidList: nil,
			})
		}
		pInfoMap[product.Name] = productDeployInfo{
			Pid:                product.Id,
			Name:               product.Name,
			UncheckServiceList: product.UncheckServiceList,
			ServiceSeq:         svcDeployInfos,
			Schema:             sc,
		}
	}
	//1.根据产品线解析产品包部署顺序
	productLineInfo, err := model.DeployProductLineList.GetProductLineListByNameAndVersion(reqParams.ProductLineName, reqParams.ProductLineVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Errorf("[Cluster->AutoOrchestration] get product line err: %v", err)
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("产品线 `%v(%v)` 不存在", reqParams.ProductLineName, reqParams.ProductLineVersion)
	}
	var resProdSeq []productDeployInfo
	deploySerial, err := GetProductLineDeploySerial(*productLineInfo)
	if err != nil {
		log.Errorf("[Cluster->AutoOrchestration] GetProductLineDeploySerial error: %v", err)
		return err
	}
	temp := map[string]struct{}{}
	for _, pInfo := range deploySerial {
		if _, ok := temp[pInfo.ProductName]; !ok {
			temp[pInfo.ProductName] = struct{}{}
			if _, ok := pInfoMap[pInfo.ProductName]; ok {
				resProdSeq = append(resProdSeq, productDeployInfo{
					Pid:                pInfoMap[pInfo.ProductName].Pid,
					Name:               pInfoMap[pInfo.ProductName].Name,
					UncheckServiceList: pInfoMap[pInfo.ProductName].UncheckServiceList,
					ServiceSeq:         pInfoMap[pInfo.ProductName].ServiceSeq,
					Schema:             pInfoMap[pInfo.ProductName].Schema,
				})
			}
		}
	}

	//以最后一次编排选择的为准 如果是第一次自动部署就插入
	err = model.ProductSelectHistory.SetPidListStrByClusterId(strings.Join(newPidList, ","), reqParams.ClusterId)
	if err != nil {
		return err
	}

	hosts, err := model.DeployHostList.GetHostListByClusterId(reqParams.ClusterId)
	var ipList []string
	for _, info := range hosts {
		ipList = append(ipList, info.Ip)
	}
	if err != nil {
		return err
	}
	// role -> sid_list
	roleHostMap := make(map[string][]string)
	hostRoleInfos, err := model.HostRole.GetRoleListByClusterId(reqParams.ClusterId)
	if err != nil {
		return err
	}
	// pid->HostRoleInfo
	idRoleMap := make(map[int]model.HostRoleInfo)
	for _, info := range hostRoleInfos {
		if _, ok := idRoleMap[info.Id]; !ok {
			idRoleMap[info.Id] = info
		}
	}

	for _, info := range hosts {
		if info.RoleList.Valid && strings.TrimSpace(info.RoleList.String) != "" {
			for _, roleIdStr := range strings.Split(info.RoleList.String, ",") {
				roleId, err := strconv.Atoi(roleIdStr)
				if err != nil {
					return err
				}
				roleName := idRoleMap[roleId].RoleName
				roleHostMap[roleName] = append(roleHostMap[roleName], info.Ip)
			}
		}
	}

	//获取服务之间的冲突和依赖关系
	err, svcRelations := model.DeployServiceRelationsList.GetServiceRelationsList()
	if err != nil {
		return err
	}

	//获取cpu、mem、disk等数据
	hostInfoList, err := model.DeployHostList.GetHostRunningInfoListByClusterId(reqParams.ClusterId)
	if err != nil {
		return err
	}
	hostInfoMap := make(map[string]model.HostRunningInfo, 0)
	for _, hostInfo := range hostInfoList {
		hostInfoMap[hostInfo.LocalIp] = hostInfo
	}

	log.Debugf("按照服务顺序主机打角色")
	//2. 按照服务顺序主机打角色
	//产品包  服务  主机  角色
	for _, info := range resProdSeq {
		for _, svc := range info.ServiceSeq {
			var maxReplica int
			//role   tag   ip
			//如果 maxReplica 未设置 则默认为 0
			if pInfoMap[info.Name].Schema.Service[svc.Name].Instance.MaxReplica == "" {
				maxReplica = 0
			} else {
				maxReplica, err = strconv.Atoi(pInfoMap[info.Name].Schema.Service[svc.Name].Instance.MaxReplica)
				if err != nil {
					return err
				}
			}
			oldIpList, err := model.DeployServiceIpList.GetServiceIpList(reqParams.ClusterId, info.Name, svc.Name)
			if err != nil {
				log.Errorf("get service ip list error:%v", err)
				return err
			}
			//没有编排记录，则进行编排。有编排记录，则无需再次进行编排
			if len(oldIpList) == 0 {
				hostList, err := selectHostByRoleAndMaxReplica(pInfoMap[info.Name].Schema.Service[svc.Name].Orchestration, roleHostMap, maxReplica, reqParams.ClusterId, ipList, info.Name, svc.Name, svcRelations, hostInfoMap)
				if err != nil {
					log.Errorf("%v", err)
					continue
				}

				//resProdSeq[prodIdx].ServiceSeq[svcIdx].SidList = hostList
				//部署A 服务的机器自动加上 A 角色
				roleHostMap[svc.Name] = hostList

				//3. 编排结果入库
				err = setIp(info.Name, svc.Name, strings.Join(hostList, ","), reqParams.ClusterId)
				if err != nil {
					return err
				}
			}
		}
	}

	//4. 返回编排结果
	type respStruct struct {
		ProductName string                                     `json:"productName"`
		Version     string                                     `json:"version"`
		Content     map[string]map[string]schema.ServiceConfig `json:"content"`
	}
	var result []respStruct
	for _, info := range resProdSeq {
		group, err := serviceGroup(info.Name, info.Schema.ProductVersion, info.UncheckServiceList, reqParams.ClusterId)
		if err != nil {
			return err
		}
		result = append(result, respStruct{
			ProductName: info.Schema.ProductName,
			Version:     info.Schema.ProductVersion,
			Content:     group,
		})
	}

	//按照首字母排序
	//sort.Slice(result, func(i, j int) bool {
	//	return result[i].ProductName < result[j].ProductName
	//})

	return result
}

//方便前端每次 set ip 后获取最新的编排结果
func AutoSvcGroup(ctx context.Context) apibase.Result {
	log.Debugf("AutoSvcGroup: %v", ctx.Request().RequestURI)
	var reqParams struct {
		ClusterId   int           `json:"cluster_id"`
		ProductInfo []productInfo `json:"product_info"`
	}
	err := ctx.ReadJSON(&reqParams)
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	// 返回编排结果
	type respStruct struct {
		ProductName string                                     `json:"productName"`
		Version     string                                     `json:"version"`
		Content     map[string]map[string]schema.ServiceConfig `json:"content"`
	}

	var result []respStruct
	for _, info := range reqParams.ProductInfo {
		pInfo, err := model.DeployProductList.GetProductInfoById(info.Id)
		if err != nil {
			log.Errorf("%v", err)
			return err
		}
		sc, err := schema.Unmarshal(pInfo.Product)
		if err != nil {
			log.Errorf("%v", err)
			return err
		}
		group, err := serviceGroup(info.Name, sc.ProductVersion, info.UncheckServiceList, reqParams.ClusterId)
		if err != nil {
			return err
		}
		result = append(result, respStruct{
			ProductName: pInfo.ProductName,
			Version:     pInfo.ProductVersion,
			Content:     group,
		})
	}
	//按照首字母排序
	sort.Slice(result, func(i, j int) bool {
		return result[i].ProductName < result[j].ProductName
	})
	return result
}

// SetAddrWithRoleInfo 设置schema ServiceAddrStruct 结构体中新加的Ip与角色信息
func SetAddrWithRoleInfo(serviceName string, sc *schema.SchemaConfig, ipRoleMap map[string]schema.IpRole, ipList string) error {
	//如果k8s类型的包 则忽略
	if sc.ProductType == "kubernetes" {
		return nil
	}

	if strings.TrimSpace(ipList) == "" {
		//填充未勾选的
		for _, ipRoleInfo := range ipRoleMap {
			sc.Service[serviceName].ServiceAddr.UnSelect = append(sc.Service[serviceName].ServiceAddr.UnSelect, ipRoleInfo)
		}
		return nil
	}

	ips := strings.Split(ipList, ",")
	////填充勾选的
	for _, ipStr := range ips {
		sc.Service[serviceName].ServiceAddr.Select = append(sc.Service[serviceName].ServiceAddr.Select, ipRoleMap[ipStr])
		//删除勾选的
		delete(ipRoleMap, ipStr)
	}
	//填充未勾选的
	for _, ipRoleInfo := range ipRoleMap {
		sc.Service[serviceName].ServiceAddr.UnSelect = append(sc.Service[serviceName].ServiceAddr.UnSelect, ipRoleInfo)
	}

	//必须对 schema 中的列表进行排序
	sort.Slice(sc.Service[serviceName].ServiceAddr.Select, func(i, j int) bool {
		return sc.Service[serviceName].ServiceAddr.Select[i].IP < sc.Service[serviceName].ServiceAddr.Select[j].IP
	})
	sort.Slice(sc.Service[serviceName].ServiceAddr.UnSelect, func(i, j int) bool {
		return sc.Service[serviceName].ServiceAddr.UnSelect[i].IP < sc.Service[serviceName].ServiceAddr.UnSelect[j].IP
	})

	return nil
}

//主机配置接口 参照原 serviceGroup  移除不相关逻辑
func serviceGroup(productName, productVersion string, uncheckedServices []string, clusterId int) (map[string]map[string]schema.ServiceConfig, error) {

	info, err := model.DeployProductList.GetByProductNameAndVersion(productName, productVersion)
	if err != nil {
		return nil, err
	}
	sc, err := schema.Unmarshal(info.Product)
	if err != nil {
		log.Errorf("SetAddrWithRoleInfo err: %v", err)
		return nil, err
	}

	err, userInfo := model.UserList.GetInfoByUserId(1)
	if err != nil {
		log.Errorf("GetInfoByUserId %v", err)
		return nil, err
	}
	reg := regexp.MustCompile(`(?i).*password.*`)

	if err = inheritBaseService(clusterId, sc, model.USE_MYSQL_DB()); err != nil {
		log.Debugf("[Product->ServiceGroup] inheritBaseService warn: %+v", err)
	}
	if err = setSchemaFieldServiceAddr(clusterId, sc, model.USE_MYSQL_DB(), ""); err != nil {
		log.Debugf("[Product->ServiceGroup] setSchemaFieldServiceAddr err: %v", err)
		return nil, err
	}
	if err = handleUncheckedServicesCore(sc, uncheckedServices); err != nil {
		log.Debugf("[Product->ServiceGroup] handleUncheckedServicesCore warn: %+v", err)
	}
	if err = sc.ParseVariable(); err != nil {
		log.Debugf("[Product->ServiceGroup] ParseVariable err: %v", err)
		return nil, err
	}

	if err = WithIpRoleInfo(clusterId, sc); err != nil {
		log.Debugf("[Product->ServiceGroup] WithIpRoleInfo err: %v", err)
		return nil, err
	}

	res := sc.Group(uncheckedServices)
	for _, group := range res {
		for _, svc := range group {
			for key, configItem := range svc.Config {
				if reg.Match([]byte(key)) {
					log.Infof("Match uncheckedServices password key %s", key)

					defaultValue, err := aes.AesEncryptByPassword(fmt.Sprintf("%s", *(configItem.(schema.VisualConfig).Default.(*string))), userInfo.PassWord)
					if err != nil {
						return nil, err
					}
					value, err := aes.AesEncryptByPassword(fmt.Sprintf("%s", *(configItem.(schema.VisualConfig).Value.(*string))), userInfo.PassWord)
					if err != nil {
						return nil, err
					}
					svc.Config[key] = schema.VisualConfig{
						Default: defaultValue,
						Desc:    configItem.(schema.VisualConfig).Desc,
						Type:    configItem.(schema.VisualConfig).Type,
						Value:   value,
					}
				}
			}
		}
	}

	return res, nil
}

//添加select unselect  信息
func WithIpRoleInfo(clusterId int, sc *schema.SchemaConfig) error {
	listByClusterId, err := model.DeployHostList.GetHostListByClusterId(clusterId)
	if err != nil {
		return err
	}

	IpRoleMap := make(map[string]schema.IpRole)
	for _, hInfo := range listByClusterId {
		if hInfo.RoleList.Valid && strings.TrimSpace(hInfo.RoleList.String) != "" {
			roleNameList, err := model.HostRole.GetRoleNameListStrByIdList(hInfo.RoleList.String)
			if err != nil {
				return err
			}
			IpRoleMap[hInfo.Ip] = schema.IpRole{
				IP:       hInfo.Ip,
				RoleList: roleNameList,
			}
		} else {
			IpRoleMap[hInfo.Ip] = schema.IpRole{
				IP:       hInfo.Ip,
				RoleList: nil,
			}
		}
	}
	for name, svc := range sc.Service {
		//每次都深拷贝 因为有 delete map操作
		deepCopyIpRoleMap := make(map[string]schema.IpRole)
		for k, v := range IpRoleMap {
			deepCopyIpRoleMap[k] = v
		}

		var ipList string
		query := "SELECT ip_list FROM " + model.DeployServiceIpList.TableName + " WHERE product_name=? AND service_name=? AND cluster_id=? AND namespace=?"
		if err := model.USE_MYSQL_DB().Get(&ipList, query, sc.ProductName, name, clusterId, ""); err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("query deployServiceIpList error: %s", err)
		}

		//if ipList != "" {
		//	ips := strings.Split(ipList, IP_LIST_SEP)
		//	var hosts []string
		//	var err error
		//	if svc.Instance != nil && !svc.Instance.UseCloud && !svc.BaseParsed {
		//		if hosts, err = getHostsFromIP(ips); err != nil {
		//			log.Errorf("get host from ip error: %v", err)
		//			hosts = ips
		//		}
		//	}
		//	sc.SetServiceAddr(name, ips, hosts)
		//
		//}
		//无论有没有 ip，都要设置 role info  因为 select 与 unselect 自动部署需要回显
		if sc.Service[name].ServiceAddr != nil {
			err = SetAddrWithRoleInfo(name, sc, deepCopyIpRoleMap, ipList)
			if err != nil {
				return err
			}
		} else {
			svc.ServiceAddr = &schema.ServiceAddrStruct{
				Host:        nil,
				IP:          nil,
				NodeId:      0,
				SingleIndex: 0,
				Select:      nil,
				UnSelect:    nil,
			}
			sc.Service[name] = svc
			err = SetAddrWithRoleInfo(name, sc, deepCopyIpRoleMap, ipList)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

//平滑升级模式，Select字段为将要执行平滑升级的主机列表，UnSelect字段为已部署了该服务的主机
func SmoothUpgradeWithIpRoleInfo(clusterId int, productInfo *model.DeployProductListInfo, sc *schema.SchemaConfig) error {
	//获取所有主机ip和角色信息
	listByClusterId, err := model.DeployHostList.GetHostListByClusterId(clusterId)
	if err != nil {
		return err
	}
	IpRoleMap := make(map[string]schema.IpRole)
	for _, hInfo := range listByClusterId {
		if hInfo.RoleList.Valid && strings.TrimSpace(hInfo.RoleList.String) != "" {
			roleNameList, err := model.HostRole.GetRoleNameListStrByIdList(hInfo.RoleList.String)
			if err != nil {
				return err
			}
			IpRoleMap[hInfo.Ip] = schema.IpRole{
				IP:       hInfo.Ip,
				RoleList: roleNameList,
			}
		} else {
			IpRoleMap[hInfo.Ip] = schema.IpRole{
				IP:       hInfo.Ip,
				RoleList: nil,
			}
		}
	}

	//可平滑升级的服务
	suMap := make(map[string]struct{})
	suList, err := upgrade.SmoothUpgrade.GetByProductName(productInfo.ProductName)
	if err != nil {
		log.Errorf("query db error: %v", err)
		return err
	}
	for _, svc := range suList {
		suMap[svc.ServiceName] = struct{}{}
	}

	for name, svc := range sc.Service {
		//每次都深拷贝 因为有 delete map操作
		deepCopyIpRoleMap := make(map[string]schema.IpRole)
		for k, v := range IpRoleMap {
			deepCopyIpRoleMap[k] = v
		}

		var ipList string
		query := "SELECT ip_list FROM " + model.DeployServiceIpList.TableName + " WHERE product_name=? AND service_name=? AND cluster_id=? AND namespace=?"
		if err := model.USE_MYSQL_DB().Get(&ipList, query, sc.ProductName, name, clusterId, ""); err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("query deployServiceIpList error: %s", err)
		}
		IpListMap := make(map[string]struct{})
		for _, v := range strings.Split(ipList, ",") {
			IpListMap[v] = struct{}{}
		}

		instanceList, count := model.DeployInstanceList.GetInstanceBelongService(productInfo.ProductName, name, clusterId)
		if count != 0 {
			//该产品包下的服务
			var nIpList, suIpList []string
			if svc.ServiceAddr == nil {
				svc.ServiceAddr = &schema.ServiceAddrStruct{
					Host:        nil,
					IP:          nil,
					NodeId:      0,
					SingleIndex: 0,
					Select:      nil,
					UnSelect:    nil,
				}
				sc.Service[name] = svc
			}
			for _, instance := range instanceList {
				//平滑升级的服务作特殊处理
				if _, ok := suMap[instance.ServiceName]; ok {
					//平滑升级的服务，根据编排信息返回Select和UnSelect
					if _, ok := IpListMap[instance.Ip]; ok {
						if _, ok := deepCopyIpRoleMap[instance.Ip]; ok {
							sc.Service[name].ServiceAddr.Select = append(sc.Service[name].ServiceAddr.Select, deepCopyIpRoleMap[instance.Ip])
						}
					} else {
						if _, ok := deepCopyIpRoleMap[instance.Ip]; ok {
							sc.Service[name].ServiceAddr.UnSelect = append(sc.Service[name].ServiceAddr.UnSelect, deepCopyIpRoleMap[instance.Ip])
						}
					}
					//已平滑升级的ip，放到ip列表中，给前端做编排限制
					if instance.Pid == productInfo.ID {
						suIpList = append(suIpList, instance.Ip)
					}
				} else {
					//不可平滑升级的服务，全部放右边
					if _, ok := deepCopyIpRoleMap[instance.Ip]; ok {
						nIpList = append(nIpList, instance.Ip)
						sc.Service[name].ServiceAddr.Select = append(sc.Service[name].ServiceAddr.Select, deepCopyIpRoleMap[instance.Ip])
					}
				}
			}
			if _, ok := suMap[name]; ok {
				sc.Service[name].ServiceAddr.IP = suIpList
			} else {
				if err = model.DeployServiceIpList.SetServiceIp(productInfo.ProductName, name, strings.Join(nIpList, IP_LIST_SEP), clusterId, productInfo.Namespace); err != nil {
					log.Errorf("SetServiceIp err: %v", err)
					return err
				}
				sc.Service[name].ServiceAddr.IP = nIpList
			}
			//必须对 schema 中的列表进行排序
			sort.Slice(sc.Service[name].ServiceAddr.Select, func(i, j int) bool {
				return sc.Service[name].ServiceAddr.Select[i].IP < sc.Service[name].ServiceAddr.Select[j].IP
			})
			sort.Slice(sc.Service[name].ServiceAddr.UnSelect, func(i, j int) bool {
				return sc.Service[name].ServiceAddr.UnSelect[i].IP < sc.Service[name].ServiceAddr.UnSelect[j].IP
			})
		} else {
			//依赖服务，编排选中的ip
			//无论有没有 ip，都要设置 role info  因为 select 与 unselect 自动部署需要回显
			if sc.Service[name].ServiceAddr != nil {
				err = SetAddrWithRoleInfo(name, sc, deepCopyIpRoleMap, ipList)
				if err != nil {
					return err
				}
			} else {
				svc.ServiceAddr = &schema.ServiceAddrStruct{
					Host:        nil,
					IP:          nil,
					NodeId:      0,
					SingleIndex: 0,
					Select:      nil,
					UnSelect:    nil,
				}
				sc.Service[name] = svc
				err = SetAddrWithRoleInfo(name, sc, deepCopyIpRoleMap, ipList)
				if err != nil {
					return err
				}
			}
			//首次平滑升级，记录平滑升级前的mysql地址
			if name == "mysql" && svc.ServiceAddr.IP != nil {
				_, err := model.DeployClusterSmoothUpgradeProductRel.GetCurrentProductByProductNameClusterId(sc.ProductName, clusterId)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					log.Errorf("%v", err)
					return err
				}
				if errors.Is(err, sql.ErrNoRows) {
					if err := model.DeployMysqlIpList.SetMysqlIp(productInfo.ProductName, strings.Join(svc.ServiceAddr.IP, IP_LIST_SEP), clusterId, productInfo.Namespace); err != nil {
						log.Errorf("SetMysqlIp err: %v", err)
						return err
					}
				}
			}
		}
	}
	return nil
}

// sid list 转为对应的 ip list
func sidList2IpList(sidList []string) ([]string, error) {
	var ipList []string
	for _, sid := range sidList {
		err, info := model.DeployHostList.GetHostInfoBySid(sid)
		if err != nil {
			return nil, err
		}
		ipList = append(ipList, info.Ip)
	}
	return ipList, nil
}

//编排结果入库  设置ip 信息
func setIp(productName, serviceName, ip string, clusterId int) error {

	err := model.DeployClusterProductRel.CheckProductReadyForDeploy(productName)
	if err != nil {
		return err
	}
	var query string
	if ip == "" {
		// delete ip
		query = "DELETE FROM " + model.DeployServiceIpList.TableName + " WHERE product_name=? AND service_name=? AND cluster_id=?"
		if _, err := model.DeployServiceIpList.GetDB().Exec(query, productName, serviceName, clusterId); err != nil {
			return err
		}
	} else {
		// 检测ip是否重复，同时排列下ip
		ipList, err := checkSortIpList(ip)
		if err != nil {
			return err
		}
		// 更新或增加服务组件和host的关联关系
		if err = model.DeployServiceIpList.SetServiceIp(productName, serviceName, strings.Join(ipList, IP_LIST_SEP), clusterId, ""); err != nil {
			log.Errorf("[SetIP] SetServiceIp err: %v", err)
			return err
		}
	}
	return nil
}

//亲和性选择
func affinitySelect(res *[]string, affinity []string, roleHostMap map[string][]string, svcRelations []model.DeployServiceRelationsInfo, productName, svcName string, maxReplica, clusterId int, hostInfoMap map[string]model.HostRunningInfo) {
	//如果 schema 中没有填或者为空，那么亲和性选择结果为空，不编排
	if len(affinity) == 0 {
		*res = (*res)[0:0]
		return
	}
	if len(affinity) == 1 {
		//1 预选
		//1.1 筛选出符合条件的主机
		matchNodeList := roleHostMap[affinity[0]]
		matchNodeMap := map[string]struct{}{}
		for _, node := range matchNodeList {
			matchNodeMap[node] = struct{}{}
		}
		//1.2 移除冲突服务所编排的主机
		conflictSelect(&matchNodeList, svcRelations, productName, svcName, clusterId, matchNodeMap)
		//1.3 符合条件的主机数量 < maxReplica，不再进行后续匹配
		if len(matchNodeList) < maxReplica {
			*res = util.IntersectionString(*res, matchNodeList)
			return
		}
		//1.4 根据依赖匹配主机
		relyOnSelect(&matchNodeList, svcRelations, productName, svcName, clusterId, matchNodeMap)
		//1.5 maxReplica = 0，匹配到的主机都安装，不再进行后续匹配; 符合条件的主机数量 < maxReplica，不再进行后续匹配
		if maxReplica == 0 || len(matchNodeList) < maxReplica {
			*res = util.IntersectionString(*res, matchNodeList)
			return
		}

		hostInfoList := make([]model.HostRunningInfo, 0)
		var load1Sum float64
		for _, ip := range matchNodeList {
			if _, ok := hostInfoMap[ip]; ok {
				hostInfoList = append(hostInfoList, hostInfoMap[ip])
				load1Sum += hostInfoMap[ip].Load1
			}
		}
		if maxReplica < len(hostInfoList) {
			rand.Seed(time.Now().UnixNano())
			rand.Shuffle(len(hostInfoList), func(i int, j int) {
				hostInfoList[i], hostInfoList[j] = hostInfoList[j], hostInfoList[i]
			})
		}
		average := load1Sum / float64(len(matchNodeList))

		//2.load1 <= average，优先分配
		expected := make([]string, 0)
		for _, v := range hostInfoList {
			if len(expected) == maxReplica {
				break
			}
			if v.Load1 <= average {
				expected = append(expected, v.LocalIp)
			}
		}

		//3.主机数量不足补齐
		if dCount := maxReplica - len(expected); dCount > 0 {
			//将找出的主机列表(差集)，进行随机排序
			difference := util.DifferenceString(matchNodeList, expected)
			if dCount < len(difference) {
				rand.Seed(time.Now().UnixNano())
				rand.Shuffle(len(difference), func(i int, j int) {
					difference[i], difference[j] = difference[j], difference[i]
				})
			}
			for index, value := range difference {
				if index == dCount {
					break
				}
				expected = append(expected, value)
			}
		}

		*res = util.IntersectionString(*res, expected)
		return
	}
	affinitySelect(res, affinity[1:], roleHostMap, svcRelations, productName, svcName, maxReplica, clusterId, hostInfoMap)
}

func conflictSelect(matchNodeList *[]string, svcRelations []model.DeployServiceRelationsInfo, productName, svcName string, clusterId int, matchNodeMap map[string]struct{}) {
	if len(*matchNodeList) == 0 {
		return
	}
	for _, relations := range svcRelations {
		if relations.RelationsType == model.RELATIONS_TYPE_CONFLICT {
			oldServiceIpList := make([]string, 0)
			var err error
			if relations.SourceProductName == productName && relations.SourceServiceName == svcName {
				//查询目标冲突服务编排信息
				oldServiceIpList, err = model.DeployServiceIpList.GetServiceIpList(clusterId, relations.TargetProductName, relations.TargetServiceName)
			} else if relations.TargetProductName == productName && relations.TargetServiceName == svcName {
				//查询来源冲突服务编排信息
				oldServiceIpList, err = model.DeployServiceIpList.GetServiceIpList(clusterId, relations.SourceProductName, relations.SourceServiceName)
			}
			if err != nil {
				log.Errorf("%v", err)
				return
			}
			//冲突服务没有编排记录，主机列表不做处理
			if len(oldServiceIpList) == 0 {
				continue
			}
			//冲突服务有编排记录，移除冲突服务ip
			conflictList := make([]string, 0)
			for _, ip := range oldServiceIpList {
				if _, ok := matchNodeMap[ip]; ok {
					conflictList = append(conflictList, ip)
				}
			}
			*matchNodeList = util.DifferenceString(*matchNodeList, conflictList)
		}
	}
}

func relyOnSelect(matchNodeList *[]string, svcRelations []model.DeployServiceRelationsInfo, productName, svcName string, clusterId int, matchNodeMap map[string]struct{}) {
	if len(*matchNodeList) == 0 {
		return
	}
	for _, relations := range svcRelations {
		if relations.RelationsType == model.RELATIONS_TYPE_RELYON {
			if relations.SourceProductName == productName && relations.SourceServiceName == svcName {
				//本服务依赖目标服务，查询目标服务编排信息
				oldServiceIpList, err := model.DeployServiceIpList.GetServiceIpList(clusterId, relations.TargetProductName, relations.TargetServiceName)
				if err != nil {
					log.Errorf("%v", err)
					return
				}
				//目标服务服务没有编排记录，本服务不再编排
				if len(oldServiceIpList) == 0 {
					*matchNodeList = (*matchNodeList)[0:0]
					return
				}
				//目标服务有编排记录，有主机角色不匹配，本服务不再编排; 主机角色全部匹配，按照亲和性编排,匹配上的主机数量 < maxReplica 后续处理
				relyOnList := make([]string, 0)
				for _, ip := range oldServiceIpList {
					if _, ok := matchNodeMap[ip]; !ok {
						*matchNodeList = (*matchNodeList)[0:0]
						return
					} else {
						relyOnList = append(relyOnList, ip)
					}
				}
				*matchNodeList = util.IntersectionString(*matchNodeList, relyOnList)
			} else if relations.TargetProductName == productName && relations.TargetServiceName == svcName {
				//本服务被来源服务所依赖，查询来源服务编排信息
				oldServiceIpList, err := model.DeployServiceIpList.GetServiceIpList(clusterId, relations.SourceProductName, relations.SourceServiceName)
				if err != nil {
					log.Errorf("%v", err)
					return
				}
				//来源服务没有编排记录，主机列表不做处理
				if len(oldServiceIpList) == 0 {
					continue
				}
				//来源服务有编排记录，有主机角色不匹配，本服务不再编排; 主机角色全部匹配，按照亲和性编排,匹配上的主机数量 < maxReplica 后续处理
				relyOnList := make([]string, 0)
				for _, ip := range oldServiceIpList {
					if _, ok := matchNodeMap[ip]; !ok {
						*matchNodeList = (*matchNodeList)[0:0]
						return
					} else {
						relyOnList = append(relyOnList, ip)
					}
				}
				*matchNodeList = util.IntersectionString(*matchNodeList, relyOnList)
			}
		}
	}
}

//反亲和性选择
func antiAffinitySelect(res *[]string, antiAffinity []string, roleHostMap map[string][]string) {
	if len(antiAffinity) == 0 {
		return
	}
	if len(antiAffinity) == 1 {
		*res = util.DifferenceString(*res, roleHostMap[antiAffinity[0]])
		return
	}
	antiAffinitySelect(res, antiAffinity[1:], roleHostMap)
}

//根据 主机角色信息与最大副本数编排服务
func selectHostByRoleAndMaxReplica(orchestration *schema.AffinityStruct, roleHostMap map[string][]string, maxReplica, clusterId int, matchIpList []string,
	productName, svcName string, svcRelations []model.DeployServiceRelationsInfo, hostInfoMap map[string]model.HostRunningInfo) ([]string, error) {
	//未设置亲和性的服务跳过编排
	if orchestration == nil || len(orchestration.Affinity) == 0 {
		return nil, fmt.Errorf("产品包：%s 服务名：%s,未设置亲和性", productName, svcName)
	}

	//亲和性选择
	affinitySelect(&matchIpList, orchestration.Affinity, roleHostMap, svcRelations, productName, svcName, maxReplica, clusterId, hostInfoMap)

	//反亲和性选择
	//antiAffinitySelect(&matchSidList, orchestration.AntiAffinity, roleHostMap)

	//如果未匹配到任何主机
	if len(matchIpList) == 0 {
		return nil, fmt.Errorf("产品包：%s 服务名：%s,未匹配到任何主机", productName, svcName)
	}

	// 如果未匹配到足够的主机
	if maxReplica > len(matchIpList) {
		return nil, fmt.Errorf("产品包：%s 服务名：%s 最大副本数为%d,但是仅仅匹配到%d台主机", productName, svcName, maxReplica, len(matchIpList))
	}

	return matchIpList, nil
}

var autoDeployContextCancelMapMutex sync.Mutex

// 用于取消自动部署
var autoDeployContextCancelMap = map[uuid.UUID]sysContext.CancelFunc{}

// AutoDeploy 自动部署  核心是循环调用单个产品包部署的方法
func AutoDeploy(ctx context.Context) apibase.Result {
	log.Debugf("AutoDeploy: %v", ctx.Request().RequestURI)
	var reqParams struct {
		ClusterId          int           `json:"cluster_id"`
		ProductLineName    string        `json:"product_line_name"`
		ProductLineVersion string        `json:"product_line_version"`
		ProductInfo        []productInfo `json:"product_info"`
	}

	err := ctx.ReadJSON(&reqParams)
	if err != nil {
		log.Errorf(" Parse reqParams err %v", err)
		return err
	}
	//检查是否没有设置 ip、冲突和依赖关系
	for _, info := range reqParams.ProductInfo {
		pInfo, err := model.DeployProductList.GetProductInfoById(info.Id)

		sc, err := schema.Unmarshal(pInfo.Product) // now product
		if err != nil {
			return err
		}
		if err = setSchemaFieldServiceAddr(reqParams.ClusterId, sc, model.USE_MYSQL_DB(), ""); err != nil {
			return err
		}
		//获取服务之间的冲突和依赖关系
		err, svcRelations := model.DeployServiceRelationsList.GetServiceRelationsList()
		if err != nil {
			return err
		}
		for _, svcName := range info.ServiceList {
			if sc.Service[svcName].ServiceAddr == nil || sc.Service[svcName].ServiceAddr.IP == nil {
				return fmt.Errorf("服务 `%v` 未完善资源分配", svcName)
			}
			if err = CheckServiceConflictAndRelyOn(reqParams.ClusterId, info.Name, svcName, svcRelations, info.UncheckServiceList); err != nil {
				log.Errorf("%v", err)
				return err
			}
		}
	}
	autoDeployUUID := uuid.NewV4()
	sysCtx, cancel := sysContext.WithCancel(sysContext.Background())
	autoDeployContextCancelMapMutex.Lock()
	autoDeployContextCancelMap[autoDeployUUID] = cancel
	autoDeployContextCancelMapMutex.Unlock()
	log.Debugf("自动部署开始 autoDeployUUID= %s ", autoDeployUUID)
	//将生成的 uuid 信息 记录到表中
	err = model.DeployUUID.InsertOne(autoDeployUUID.String(), "", model.AutoDeployUUIDType, 0)
	if err != nil {
		return nil
	}
	userId, err := ctx.Values().GetInt("userId")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	go func(sysCtx sysContext.Context, autoDeployUUID uuid.UUID) {

		select {
		case <-sysCtx.Done(): //取出值即说明是结束信号
			log.Debugf("取消自动部署,autoDeployUUID= %s ", autoDeployUUID)
			return
		default:
			{
				//根据产品线解析产品包部署顺序
				productLineInfo, err := model.DeployProductLineList.GetProductLineListByNameAndVersion(reqParams.ProductLineName, reqParams.ProductLineVersion)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					log.Errorf("[Cluster->AutoDeploy] get product line err: %v", err)
					return
				}
				if errors.Is(err, sql.ErrNoRows) {
					log.Errorf("产品线 `%v(%v)` 不存在", reqParams.ProductLineName, reqParams.ProductLineVersion)
					return
				}
				var productDeployInfos []productDeployInfo
				deploySerial, err := GetProductLineDeploySerial(*productLineInfo)
				if err != nil {
					log.Errorf("[Cluster->AutoDeploy] GetProductLineDeploySerial error: %v", err)
					return
				}

				for _, pInfo := range deploySerial {
					for _, product := range reqParams.ProductInfo {
						if pInfo.ProductName == product.Name {
							info, err := model.DeployProductList.GetProductInfoById(product.Id)
							if err != nil {
								log.Errorf("%v", err)
								return
							}
							var svcDeployInfos []svcDeployInfo
							sc, err := schema.Unmarshal(info.Product)
							if err != nil {
								log.Errorf("%v", err)
								return
							}
							for _, name := range product.ServiceList {
								svcDeployInfos = append(svcDeployInfos, svcDeployInfo{
									Name:    name,
									SidList: nil,
								})
							}
							productDeployInfos = append(productDeployInfos, productDeployInfo{
								Pid:                product.Id,
								Name:               product.Name,
								UncheckServiceList: product.UncheckServiceList,
								ServiceSeq:         svcDeployInfos,
								Schema:             sc,
							})
						}
					}
				}

				var pidSeq []string
				for _, info := range productDeployInfos {
					pidSeq = append(pidSeq, strconv.Itoa(info.Pid))
				}

				//将本次实际部署的 pidList 入库 与 autoDeployUUID 关联  当取消自动部署的时候，关联出 pidList
				err = model.DeployUUID.SetPidByUUID(autoDeployUUID.String(), strings.Join(pidSeq, ","))
				if err != nil {
					return
				}

				for _, info := range productDeployInfos {
					infoById, err := model.DeployProductList.GetProductInfoById(info.Pid)
					if err != nil {
						return
					}
					productUUID := uuid.NewV4()
					err = model.DeployUUID.InsertOne(productUUID.String(), autoDeployUUID.String(), model.AutoDeployChildrenUUIDType, info.Pid)
					if err != nil {
						return
					}
					log.Debugf("正在自动部署 %s", infoById.ProductName)
					//传入 parentCtx 当parentCtx停止时，由其生成的子 context 都将退出
					dealDeployRes := autoDealDeploy(ctx, sysCtx, infoById.ProductName, infoById.ProductVersion, info.UncheckServiceList, userId, reqParams.ClusterId, productUUID)
					log.Debugf("%s自动部署完成", infoById.ProductName)
					if _, ok := dealDeployRes.(error); ok {
						log.Errorf("%s自动部署失败", infoById.ProductName)
						return
					}
				}

				log.Debugf("自动部署全部完成 autoDeployUUID= %s ", autoDeployUUID)
				return
			}
		}
	}(sysCtx, autoDeployUUID)
	return map[string]interface{}{"deploy_uuid": autoDeployUUID}
}

func CheckServiceConflictAndRelyOn(clusterId int, productName, serviceName string, svcRelations []model.DeployServiceRelationsInfo, uncheckedServices []string) error {
	serviceIpList, err := model.DeployServiceIpList.GetServiceIpList(clusterId, productName, serviceName)
	if err != nil {
		return err
	}

	for _, relations := range svcRelations {
		var err error
		var oldServiceIpList = make([]string, 0)
		var oldServiceName string
		if relations.SourceProductName == productName && relations.SourceServiceName == serviceName {
			oldServiceIpList, err = model.DeployServiceIpList.GetServiceIpList(clusterId, relations.TargetProductName, relations.TargetServiceName)
			oldServiceName = relations.TargetServiceName
		} else if relations.TargetProductName == productName && relations.TargetServiceName == serviceName {
			oldServiceIpList, err = model.DeployServiceIpList.GetServiceIpList(clusterId, relations.SourceProductName, relations.SourceServiceName)
			oldServiceName = relations.SourceServiceName
		}
		if err != nil {
			log.Errorf("%v", err)
			return err
		}
		if !util.StringContain(uncheckedServices, oldServiceName) && relations.RelationsType == model.RELATIONS_TYPE_CONFLICT && len(oldServiceIpList) > 0 {
			conflictList := util.IntersectionString(serviceIpList, oldServiceIpList)
			if len(conflictList) > 0 {
				return fmt.Errorf("存在部署冲突！`%v` 只允许编排 `%v` 所在主机范围外的主机", serviceName, oldServiceName)
			}
		} else if !util.StringContain(uncheckedServices, oldServiceName) && relations.RelationsType == model.RELATIONS_TYPE_RELYON && len(oldServiceIpList) > 0 {
			relyOnList := util.IntersectionString(serviceIpList, oldServiceIpList)
			if len(relyOnList) == 0 {
				return fmt.Errorf("存在部署依赖！`%v` 只允许编排 `%v` 所在主机范围内的主机", serviceName, oldServiceName)
			}
		}
	}

	return nil
}

// AutoDeployCancel 取消自动部署过程
func AutoDeployCancel(ctx context.Context) apibase.Result {
	var reqParams struct {
		ClusterId  int    `json:"cluster_id"`
		DeployUUID string `json:"deploy_uuid"`
	}
	err := ctx.ReadJSON(&reqParams)
	if err != nil {
		log.Errorf("parse reqParams err: %v", err)
		return err
	}

	uuidInfo, err := model.DeployUUID.GetInfoByUUID(reqParams.DeployUUID)
	if err != nil {
		return err
	}
	//如果是自动部署的子产品 uuid 那么根据子 uuid 查询到自动部署 uuid
	if uuidInfo.UuidType == model.AutoDeployChildrenUUIDType {
		return autoDeployModelCancel(uuidInfo.ParentUUID, err, reqParams.ClusterId)
	}
	return autoDeployModelCancel(reqParams.DeployUUID, err, reqParams.ClusterId)
}

//使用 autoDeployUUID 停止自动部署流程
func autoDeployModelCancel(autoDeployUUID string, err error, clusterId int) error {
	deployUUID, _ := uuid.FromString(autoDeployUUID)
	autoDeployContextCancelMapMutex.Lock()
	if autoCancel, ok := autoDeployContextCancelMap[deployUUID]; ok {
		autoCancel()
	}
	autoDeployContextCancelMapMutex.Unlock()

	autoDeployInfo, err := model.DeployUUID.GetInfoByUUID(autoDeployUUID)
	if err != nil {
		return err
	}
	//拿到本次自动部署涉及到的所有 pid
	for _, pidStr := range strings.Split(autoDeployInfo.Pid, ",") {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return err
		}
		//todo 这里筛选所有的可能会有问题  已经部署过的怎么办
		instances, err := model.DeployInstanceList.GetInstanceListByClusterId(clusterId, pid)
		if err != nil {
			return err
		}
		for _, ins := range instances {
			//params.Agents[ins.Sid] = append(params.Agents[ins.Sid], ins.AgentId)

			// cancel health check
			ev := &event.Event{
				AgentId: ins.AgentId,
				Type:    event.REPORT_EVENT_HEALTH_CHECK_CANCEL,
				Data:    nil,
			}
			event.GetEventManager().EventReciever(ev)
		}
	}
	return nil
}

//部署单个产品包
func autoDealDeploy(ctx context.Context, parentCtx sysContext.Context, productName, productVersion string, uncheckedServices []string, userId, clusterId int, deployUUID uuid.UUID) (rlt interface{}) {
	log.Infof("deploy product_name:%v, product_version: %v, userId: %v, clusterId: %v", productName, productVersion, userId, clusterId)
	cluster, err := model.DeployClusterList.GetClusterInfoById(clusterId)
	if err != nil {
		return err
	}
	defer func() {
		if err := addSafetyAuditRecord(ctx, "部署向导", "产品部署", "集群名称："+cluster.Name+", 组件名称："+productName+productVersion); err != nil {
			log.Errorf("failed to add safety audit record\n")
		}
	}()
	tx := model.USE_MYSQL_DB().MustBegin()
	defer func() {
		if _, ok := rlt.(error); ok {
			tx.Rollback()
		}
		if r := recover(); r != nil {
			tx.Rollback()
			rlt = r
		}
	}()
	var productListInfo model.DeployProductListInfo
	query := "SELECT id, product, parent_product_name FROM " + model.DeployProductList.TableName + " WHERE product_name=? AND product_version=?"
	if err := tx.Get(&productListInfo, query, productName, productVersion); err != nil {
		return err
	}

	sc, err := schema.Unmarshal(productListInfo.Product) // now product
	if err != nil {
		return err
	}
	if err = inheritBaseService(clusterId, sc, tx); err != nil {
		return err
	}
	if err = setSchemaFieldServiceAddr(clusterId, sc, tx, ""); err != nil {
		return err
	}
	if err = handleUncheckedServicesCore(sc, uncheckedServices); err != nil {
		return err
	}
	if err = sc.CheckServiceAddr(); err != nil {
		log.Errorf("%v", err)
		return err
	}
	err = model.DeployClusterProductRel.CheckProductReadyForDeploy(productName)
	if err != nil {
		return err
	}

	rel := model.ClusterProductRel{
		Pid:        productListInfo.ID,
		ClusterId:  clusterId,
		Status:     model.PRODUCT_STATUS_DEPLOYING,
		DeployUUID: deployUUID.String(),
		UserId:     userId,
	}
	oldProductListInfo, err := model.DeployClusterProductRel.GetCurrentProductByProductNameClusterId(productName, clusterId)
	if err == nil {
		query = "UPDATE " + model.DeployClusterProductRel.TableName + " SET pid=?, user_id=?, `status`=?, `deploy_uuid`=?, deploy_time=NOW() WHERE pid=? AND clusterId=? AND is_deleted=0"
		if _, err := tx.Exec(query, productListInfo.ID, userId, model.PRODUCT_STATUS_DEPLOYING, deployUUID, oldProductListInfo.ID, clusterId); err != nil {
			log.Errorf("%v", err)
			return err
		}
	} else if err == sql.ErrNoRows {
		query = "INSERT INTO " + model.DeployClusterProductRel.TableName + " (pid, clusterId, deploy_uuid, user_id, deploy_time, status) VALUES" +
			" (:pid, :clusterId, :deploy_uuid, :user_id, NOW(), :status)"
		if _, err = tx.NamedExec(query, &rel); err != nil {
			log.Errorf("%v", err)
			return err
		}
	} else {
		log.Errorf("%v", err)
		return err
	}

	if len(uncheckedServices) > 0 {
		uncheckedServiceInfo := model.DeployUncheckedServiceInfo{ClusterId: clusterId, Pid: productListInfo.ID, UncheckedServices: strings.Join(uncheckedServices, ",")}
		query = "INSERT INTO " + model.DeployUncheckedService.TableName + " (pid, cluster_id, unchecked_services) VALUES" +
			" (:pid, :cluster_id, :unchecked_services) ON DUPLICATE KEY UPDATE unchecked_services=:unchecked_services, update_time=NOW()"
		if _, err = tx.NamedExec(query, &uncheckedServiceInfo); err != nil {
			log.Errorf("%v", err)
			return err
		}
	} else {
		query = "DELETE FROM " + model.DeployUncheckedService.TableName + " WHERE pid=? AND cluster_id=?"
		if _, err = tx.Exec(query, productListInfo.ID, clusterId); err != nil && err != sql.ErrNoRows {
			log.Errorf("%v", err)
			return err
		}
	}

	productHistoryInfo := model.DeployProductHistoryInfo{
		ClusterId:          clusterId,
		DeployUUID:         deployUUID,
		ProductName:        productName,
		ProductNameDisplay: productListInfo.ProductNameDisplay,
		ProductVersion:     productVersion,
		Status:             model.PRODUCT_STATUS_DEPLOYING,
		ParentProductName:  productListInfo.ParentProductName,
		UserId:             userId,
	}
	sc.ParentProductName = productListInfo.ParentProductName

	query = "INSERT INTO " + model.DeployProductHistory.TableName + " (cluster_id, product_name, product_name_display, deploy_uuid, product_version, `status`, parent_product_name, deploy_start_time, user_id) " +
		"VALUES (:cluster_id, :product_name, :product_name_display, :deploy_uuid, :product_version, :status , :parent_product_name, NOW(), :user_id)"
	if _, err := tx.NamedExec(query, &productHistoryInfo); err != nil {
		log.Errorf("%v", err)
		return err
	}

	if err := tx.Commit(); err != nil {
		log.Errorf("%v", err)
		return err
	}

	childrenCtx, cancel := sysContext.WithCancel(parentCtx)
	contextCancelMapMutex.Lock()
	contextCancelMap[deployUUID] = cancel
	contextCancelMapMutex.Unlock()

	//生成 operationid 并且落库
	operationId := uuid.NewV4().String()
	err = model.OperationList.Insert(model.OperationInfo{
		ClusterId:       clusterId,
		OperationId:     operationId,
		OperationType:   enums.OperationType.ProductDeploy.Code,
		OperationStatus: enums.ExecStatusType.Running.Code,
		ObjectType:      enums.OperationObjType.Product.Code,
		ObjectValue:     productName,
	})
	if err != nil {
		log.Errorf("OperationList Insert err:%v", err)
	}

	//todo: opid
	deploy(sc, deployUUID, productListInfo.ID, childrenCtx, uncheckedServices, clusterId, 0, operationId, "", false)

	return nil
}
func getAllsvc(ctx context.Context) apibase.Result {
	log.Debugf("Service: %v", ctx.Request().RequestURI)

	paramErrs := apibase.NewApiParameterErrors()
	productName := ctx.Params().Get("product_name")
	productVersion := ctx.Params().Get("product_version")
	baseClusterId := ctx.URLParam("baseClusterId")
	if productName == "" {
		paramErrs.AppendError("$", fmt.Errorf("product_name is empty"))
	}
	if productVersion == "" {
		paramErrs.AppendError("$", fmt.Errorf("product_version is empty"))
	}
	clusterId, err := GetCurrentClusterId(ctx)
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	paramErrs.CheckAndThrowApiParameterErrors()

	info, err := model.DeployProductList.GetByProductNameAndVersion(productName, productVersion)
	if err != nil {
		return err
	}

	sc, err := schema.Unmarshal(info.Product)
	if err != nil {
		log.Errorf("[Product->Service] Unmarshal err: %v", err)
		return err
	}

	if baseClusterId != "" {
		clusterId, err = strconv.Atoi(baseClusterId)
		if err != nil {
			return err
		}
	}
	// 获取该产品包下服务组件依赖对应服务的相关配置信息
	if err = inheritBaseService(clusterId, sc, model.USE_MYSQL_DB()); err != nil {
		log.Errorf("[Product->Service] inheritBaseService warn: %+v", err)
	}

	services := []map[string]string{}
	for name, svc := range sc.Service {
		serviceDisplay := svc.ServiceDisplay
		if serviceDisplay == "" {
			serviceDisplay = name
		}
		services = append(services, map[string]string{
			"serviceName":        name,
			"serviceNameDisplay": serviceDisplay,
			"serviceVersion":     svc.Version,
			"baseProduct":        svc.BaseProduct,
			"baseService":        svc.BaseService,
		})
	}

	return services
}

// OrchestrationHistory 返回编排历史 deploy_service_ip_list 表中只要有编排结果就要回显
// 这里维护 ProductSelectHistory 中的 pid_list 字段 而不是采用已经使用 deploy_service_ip_list 与 deploy_cluster_product_rel
// 因为deploy_service_ip_list 无法对应到不同的 version  deploy_cluster_product_rel 无法回显只编排而不部署的产品包
func OrchestrationHistory(ctx context.Context) apibase.Result {
	type svcInfo struct {
		ServiceName        string `json:"service_name"`
		ServiceNameDisplay string `json:"service_name_display"`
		ServiceVersion     string `json:"service_version"`
		BaseProduct        string `json:"base_product"`
		BaseService        string `json:"base_service"`
	}
	type svcStruct struct {
		CheckSvc   []svcInfo           `json:"check_service,omitempty"`
		UnCheckSvc []svcInfo           `json:"uncheck_service,omitempty"`
		AllSvc     []map[string]string `json:"all_service,omitempty"`
	}
	type respStruct struct {
		Pid         int       `json:"pid"`
		ProductName string    `json:"product_name"`
		Service     svcStruct `json:"service"`
	}

	clusterIdStr := ctx.URLParam("cluster_id")
	clusterId, err := strconv.Atoi(clusterIdStr)
	if err != nil {
		return err
	}
	//编排结果回显
	pidListStr, err := model.ProductSelectHistory.GetPidListStrByClusterId(clusterId)
	if errors.Is(err, sql.ErrNoRows) {
		log.Debugf("未查询到自动编排历史")
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.TrimSpace(pidListStr) == "" {
		return nil
	}
	pidList := strings.Split(pidListStr, ",")
	var resp []respStruct
	for _, pidStr := range pidList {
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return err
		}
		productInfo, err := model.DeployProductList.GetProductInfoById(pid)
		if err != nil {
			return err
		}
		sc, err := schema.Unmarshal(productInfo.Schema)
		if err != nil {
			return err
		}
		uncheckedServiceInfo, err := model.DeployUncheckedService.GetUncheckedServicesByPidClusterId(pid, clusterId, "")
		if err != nil {
			return err
		}
		var uncheckedSvc []string
		if strings.TrimSpace(uncheckedServiceInfo.UncheckedServices) != "" {
			uncheckedSvc = strings.Split(uncheckedServiceInfo.UncheckedServices, ",")
		}
		var checkSvc, unCheckSvc []svcInfo
		var allSvc []map[string]string
		for svcName, config := range sc.Service {
			//组合 allSvc
			serviceDisplay := config.ServiceDisplay
			if serviceDisplay == "" {
				serviceDisplay = svcName
			}
			info := svcInfo{
				ServiceName:        svcName,
				ServiceNameDisplay: serviceDisplay,
				ServiceVersion:     config.Version,
				BaseProduct:        config.BaseProduct,
				BaseService:        config.BaseService,
			}
			//不能用 svcinfo struct 因为前端这块对字段要求驼峰
			// EM 目前json变量名下划线驼峰都有，非常乱，建议以后的统一都走下划线
			allSvc = append(allSvc, map[string]string{
				"serviceName":        info.ServiceName,
				"serviceNameDisplay": info.ServiceNameDisplay,
				"serviceVersion":     info.ServiceVersion,
				"baseProduct":        info.BaseProduct,
				"baseService":        info.BaseService,
			})

			//组合 uncheckSvc
			if util.IndexOfString(uncheckedSvc, svcName) != -1 && config.BaseProduct == "" {
				unCheckSvc = append(unCheckSvc, info)
				continue
			}

			//组合勾选的
			if util.IndexOfString(uncheckedSvc, svcName) == -1 && config.BaseProduct == "" {
				checkSvc = append(checkSvc, info)
			}

		}
		resp = append(resp, respStruct{
			Pid:         pid,
			ProductName: productInfo.ProductName,
			Service: svcStruct{
				CheckSvc:   checkSvc,
				UnCheckSvc: unCheckSvc,
				AllSvc:     allSvc,
			},
		})
	}
	return resp

}
func GetHostClusterHostList(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->GetHostClusterList] GetHostClusterList from EasyMatrix API ")

	paramErrs := apibase.NewApiParameterErrors()
	clusterId := ctx.URLParam("cluster_id")
	if clusterId == "" {
		paramErrs.AppendError("$", fmt.Errorf("clusterId is empty"))
	}
	paramErrs.CheckAndThrowApiParameterErrors()

	group := ctx.URLParam("group")
	hostOrIp := ctx.URLParam("host_or_ip")
	isRunning := ctx.URLParam("is_running")
	status := ctx.URLParam("status")
	parentProductName := ctx.URLParam("parent_product_name")

	values := []interface{}{"%" + hostOrIp + "%", "%" + hostOrIp + "%", clusterId, 0}
	whereCause := ` AND deploy_cluster_host_rel.clusterId=? AND deploy_cluster_host_rel.is_deleted=? `

	//主机分组筛选
	if group != "" {
		whereCause += ` AND deploy_host.group IN (`
		for i, v := range strings.Split(group, ",") {
			if i > 0 {
				whereCause += `,`
			}
			whereCause += `?`
			values = append(values, v)
		}
		whereCause += `)`
	}

	//产品名筛选
	if parentProductName != "" {
		whereCause += ` AND deploy_product_list.parent_product_name=?`
		values = append(values, parentProductName)
	}

	//errMssg筛选
	if status != "" {
		whereCause += ` AND deploy_host.errorMsg IN (`
		for i, v := range strings.Split(status, ",") {
			if i > 0 {
				whereCause += `,`
			}
			whereCause += `?`
			values = append(values, v)
		}
		whereCause += `)`
	}
	//is_running筛选
	ret := strings.Split(isRunning, ",")
	if len(isRunning) > 0 && len(ret) == 1 {
		if isRunning == "false" {
			whereCause += " AND TIMESTAMPDIFF( MINUTE, deploy_host.updated, NOW()) >= 3"
		} else if isRunning == "true" {
			whereCause += " AND TIMESTAMPDIFF( MINUTE, deploy_host.updated, NOW()) < 3"
		}
	}
	// 由表deploy_cluster_host_rel开始左连接
	baseQuery := fmt.Sprintf(`FROM deploy_cluster_host_rel
LEFT JOIN deploy_host ON deploy_cluster_host_rel.sid = deploy_host.sid
LEFT JOIN deploy_instance_list ON deploy_host.sid = deploy_instance_list.sid
LEFT JOIN deploy_product_list ON deploy_instance_list.pid = deploy_product_list.id
LEFT JOIN sidecar_list ON sidecar_list.id = deploy_host.sid
WHERE deploy_host.sid != '' AND deploy_host.isDeleted=0 AND (deploy_host.hostname LIKE ? OR deploy_host.ip LIKE ?)%s`, whereCause)

	// 复用 api/v2/agent/hosts部分代码
	type hostInfo struct {
		model.HostInfo
		RunUser                string                  `json:"run_user"`
		ProductNameList        string                  `json:"product_name_list" db:"product_name_list"`
		ProductNameDisplayList string                  `json:"product_name_display_list" db:"product_name_display_list"`
		ProductIdList          string                  `json:"pid_list" db:"pid_list"`
		MemSize                int64                   `json:"mem_size" db:"mem_size"`
		MemUsage               int64                   `json:"mem_usage" db:"mem_usage"`
		CpuCores               int                     `json:"-" db:"cpu_cores"`
		DiskUsage              sql.NullString          `json:"disk_usage" db:"disk_usage"`
		NetUsage               sql.NullString          `json:"net_usage" db:"net_usage"`
		MemSizeDisplay         string                  `json:"mem_size_display"`
		MemUsedDisplay         string                  `json:"mem_used_display"`
		DiskSizeDisplay        string                  `json:"disk_size_display"`
		DiskUsedDisplay        string                  `json:"disk_used_display"`
		FileSizeDisplay        string                  `json:"file_size_display"`
		FileUsedDisplay        string                  `json:"file_used_display"`
		CpuCoreSizeDisplay     string                  `json:"cpu_core_size_display"`
		CpuCoreUsedDisplay     string                  `json:"cpu_core_used_display"`
		NetUsageDisplay        []model.NetUsageDisplay `json:"net_usage_display,omitempty"`
		RoleListDisplay        []struct {
			Id       int    `json:"role_id"`
			RoleName string `json:"role_name"`
		} `json:"role_list_display,omitempty"`
		ExecId       string  `json:"exec_id"`
		IsRunning    bool    `json:"is_running" db:"is_running"`
		CpuUsagePct  float64 `json:"cpu_usage_pct" db:"cpu_usage_pct"`
		MemUsagePct  float64 `json:"mem_usage_pct" db:"mem_usage_pct"`
		DiskUsagePct float64 `json:"disk_usage_pct" db:"disk_usage_pct"`
		Alert        bool    `json:"alert"`
	}

	var count int
	hostsList := make([]hostInfo, 0)
	query := "SELECT COUNT(DISTINCT deploy_host.sid) " + baseQuery
	whoamiCmd := "#!/bin/sh\n whoami"
	if err := model.USE_MYSQL_DB().Get(&count, query, values...); err != nil {
		log.Errorf("AgentHosts count query: %v, values %v, err: %v", query, values, err)
		apibase.ThrowDBModelError(err)
	}
	if count > 0 {
		//查询agent is_running状态
		isRunningStr := "IF(TIMESTAMPDIFF(MINUTE,deploy_host.updated,NOW())<3,true,false) as is_running, "
		if len(isRunning) > 0 && len(ret) == 1 {
			if isRunning == "false" {
				isRunningStr = "false as is_running, "
			} else if isRunning == "true" {
				isRunningStr += "true as is_running, "
			}
		}
		query = "SELECT deploy_host.*," + isRunningStr +
			"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.product_name)),'') AS product_name_list, " +
			"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.product_name_display)),'') AS product_name_display_list, " +
			"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.id)),'') AS pid_list," +
			"sidecar_list.mem_size, sidecar_list.mem_usage, sidecar_list.disk_usage, sidecar_list.net_usage, " +
			"sidecar_list.cpu_cores, sidecar_list.cpu_usage as cpu_usage_pct, sidecar_list.mem_usage/sidecar_list.mem_size as mem_usage_pct, sidecar_list.disk_usage_pct " +
			baseQuery + " GROUP BY deploy_host.sid " + apibase.GetPaginationFromQueryParameters(nil, ctx, model.HostInfo{}).AsQuery()
		if err := model.USE_MYSQL_DB().Select(&hostsList, query, values...); err != nil {
			log.Errorf("AgentHosts query: %v, values %v, err: %v", query, values, err)
			apibase.ThrowDBModelError(err)
		}
		for i, list := range hostsList {
			hostsList[i].MemSizeDisplay, hostsList[i].MemUsedDisplay = MultiSizeConvert(list.MemSize, list.MemUsage)
			hostsList[i].CpuCoreUsedDisplay = strconv.FormatFloat(list.CpuUsagePct*float64(list.CpuCores)/100, 'f', 2, 64) + "core"
			hostsList[i].CpuCoreSizeDisplay = strconv.Itoa(list.CpuCores) + "core"

			if list.DiskUsage.Valid {
				hostsList[i].DiskSizeDisplay, hostsList[i].DiskUsedDisplay, hostsList[i].FileSizeDisplay, hostsList[i].FileUsedDisplay = diskUsageConvert(list.DiskUsage.String)
			}
			if list.NetUsage.Valid {
				hostsList[i].NetUsageDisplay = netUsageConvert(list.NetUsage.String)
			}
			if list.IsDeleted == 0 && list.Status > 0 && hostsList[i].IsRunning {
				content, err := agent.AgentClient.ToExecCmdWithTimeout(list.SidecarId, "", whoamiCmd, "5s", "", "")
				if err != nil {
					//exec failed
					content = err.Error()
				}
				user := strings.Replace(content, LINUX_SYSTEM_LINES, "", -1)
				hostsList[i].RunUser = user
			}
			err, dashboardResp := grafana.GetDashboardByUid("Ne_roaViz")
			alertList := ServiceAlertList(strconv.Itoa(dashboardResp.Dashboard.Id), list.Ip)
			var isAlert bool
			for _, alert := range alertList {
				if alert.State != "ok" && alert.State != "paused" && alert.State != "pending" {
					isAlert = true
					break
				}
			}
			if list.Status < 0 || hostsList[i].IsRunning == false {
				hostsList[i].Alert = true
			} else if isAlert == true {
				hostsList[i].Alert = true
			}
			//附加角色信息
			if list.RoleList.Valid && strings.TrimSpace(list.RoleList.String) != "" {
				log.Infof("list.RoleList.Valid true RoleListDisplay %+v", strings.Split(list.RoleList.String, ","))
				for _, roleId := range strings.Split(list.RoleList.String, ",") {
					rid, err := strconv.Atoi(roleId)
					if err != nil {
						return err
					}
					h, err := model.HostRole.GetRoleInfoById(rid)
					if err != nil {
						return err
					}
					hostsList[i].RoleListDisplay = append(hostsList[i].RoleListDisplay, struct {
						Id       int    `json:"role_id"`
						RoleName string `json:"role_name"`
					}{Id: rid, RoleName: h.RoleName})
				}
			} else {
				hostsList[i].RoleListDisplay = nil
			}
			//	附加 execId 信息
			operationInfo, err := model.OperationList.GetByOperationTypeAndObjectValue(enums.OperationType.HostInit.Code, hostsList[i].Ip)
			if errors.Is(err, sql.ErrNoRows) {
				log.Errorf("未查询到 operationId sid: %s", hostsList[i].SidecarId)
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Errorf(" OperationList.GetByOperationTypeAndObjectValue error: %v", err)
			}
			execShellInfo, err := model.ExecShellList.GetByOperationId(operationInfo.OperationId)
			if errors.Is(err, sql.ErrNoRows) {
				log.Errorf("未查询到 operationId sid: %s", hostsList[i].SidecarId)
				continue
			}
			if err != nil {
				log.Errorf("ExecShellList.GetByOperationId error: %v", err)
				continue
			}
			hostsList[i].ExecId = execShellInfo.ExecId
		}
	}
	return map[string]interface{}{
		"hosts": hostsList,
		"count": count,
	}
}

func GetHostClusterOverView(ctx context.Context) apibase.Result {
	log.Debugf("[Cluster->GetHostClusterOverView] GetHostClusterOverView from EasyMatrix API ")
	id, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	cluster, err := model.DeployClusterList.GetClusterInfoById(id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}

	//获取cpu、mem、disk等数据
	query := "SELECT deploy_host.id,mem_size,mem_usage,disk_usage,cpu_cores,cpu_usage,local_ip,deploy_host.status,deploy_host.updated " +
		"FROM deploy_cluster_host_rel " +
		"LEFT JOIN deploy_host ON deploy_cluster_host_rel.sid = deploy_host.sid " +
		"LEFT JOIN sidecar_list ON sidecar_list.id = deploy_host.sid " +
		"WHERE deploy_cluster_host_rel.clusterId = ? and deploy_cluster_host_rel.is_deleted = 0"
	type HostInfo struct {
		MemSize     int64          `db:"mem_size"`
		MemUsage    int64          `db:"mem_usage"`
		DiskUsage   sql.NullString `db:"disk_usage"`
		CpuCores    int            `db:"cpu_cores"`
		CpuUsage    float64        `db:"cpu_usage"`
		Status      int            `db:"status"`
		LocalIp     string         `db:"local_ip"`
		Id          int            `db:"id"`
		Updated     base.Time      `db:"updated"`
		CpuUsagePct float64
		MemUsagePct float64
	}
	hostList := make([]HostInfo, 0)
	if err := model.USE_MYSQL_DB().Select(&hostList, query, cluster.Id); err != nil {
		return fmt.Errorf("Database err: %v", err)
	}

	var cpuUsage float64
	var cpuSize, count, errorNodes int
	var memSize, memUsage, diskSize, diskUsed int64

	// 累加数值、求百分比
	for i := range hostList {
		var diskUsages []model.DiskUsage // 累加计算disk
		if err := json.Unmarshal([]byte(hostList[i].DiskUsage.String), &diskUsages); err != nil {
			log.Errorf(err.Error())
		}
		for _, diskUsage := range diskUsages {
			if diskUsage.MountPoint != "/" {
				// include fileSize/Used
				diskSize += int64(diskUsage.TotalSpace)
				diskUsed += int64(diskUsage.UsedSpace)
			} else {
				diskSize += int64(diskUsage.TotalSpace)
				diskUsed += int64(diskUsage.UsedSpace)
			}
		}
		if hostList[i].Status != 3 || time.Now().Sub(time.Time(hostList[i].Updated)) > 3*time.Minute {
			errorNodes++
		}
		memSize += hostList[i].MemSize
		memUsage += hostList[i].MemUsage
		cpuSize += hostList[i].CpuCores
		cpuUsage += hostList[i].CpuUsage * float64(hostList[i].CpuCores) / 100
		hostList[i].CpuUsagePct = hostList[i].CpuUsage
		hostList[i].MemUsagePct = float64(hostList[i].MemUsage) / float64(hostList[i].MemSize) * 100
	}

	// top5排序
	cpuTop := make([]map[string]interface{}, 0)
	sort.SliceStable(hostList, func(i, j int) bool {
		return hostList[i].CpuUsagePct > hostList[j].CpuUsagePct
	})
	for _, v := range hostList {
		if count >= 5 {
			break
		}
		cpuTop = append(cpuTop, map[string]interface{}{
			"ip":    v.LocalIp,
			"id":    v.Id,
			"usage": strconv.FormatFloat(v.CpuUsagePct, 'f', 2, 64),
		})
		count++
	}

	count = 0
	memTop := make([]map[string]interface{}, 0)
	sort.SliceStable(hostList, func(i, j int) bool {
		return hostList[i].MemUsagePct > hostList[j].MemUsagePct
	})
	for _, v := range hostList {
		if count >= 5 {
			break
		}
		memTop = append(memTop, map[string]interface{}{
			"ip":    v.LocalIp,
			"id":    v.Id,
			"usage": strconv.FormatFloat(v.MemUsagePct, 'f', 2, 64),
		})
		count++
	}
	memSizeDisplay, memUsedDisplay := MultiSizeConvert(memSize, memUsage)
	diskSizeDisplay, diskUsedDisplay := MultiSizeConvert(diskSize, diskUsed)
	return map[string]interface{}{
		"mode":              cluster.Mode,
		"create_time":       cluster.CreateTime.Time,
		"create_user":       cluster.CreateUser,
		"nodes":             len(hostList),
		"mem_size_display":  memSizeDisplay,
		"mem_used_display":  memUsedDisplay,
		"disk_size_display": diskSizeDisplay,
		"disk_used_display": diskUsedDisplay,
		"cpu_size_display":  strconv.Itoa(cpuSize) + "core",
		"cpu_used_display":  strconv.FormatFloat(cpuUsage, 'f', 2, 64) + "core",
		"error_nodes":       errorNodes,
		"metrics": map[string]interface{}{
			"cpu_top5": cpuTop,
			"mem_top5": memTop,
		},
	}

}

func GetHostClusterPerformance(ctx context.Context) apibase.Result {
	log.Debugf("HostClusterPerformance: %v", ctx.Request().RequestURI)

	paramErrs := apibase.NewApiParameterErrors()
	clusterId := ctx.Params().Get("cluster_id")
	if clusterId == "" {
		paramErrs.AppendError("$", fmt.Errorf("clusterId is empty"))
	}
	metric := ctx.URLParam("metric")
	if metric == "" {
		paramErrs.AppendError("$", fmt.Errorf("metric is empty"))
	}
	fromTime, err := ctx.URLParamInt64("from")
	if err != nil {
		paramErrs.AppendError("$", fmt.Errorf("from is empty"))
	}
	toTime, err := ctx.URLParamInt64("to")
	if err != nil {
		paramErrs.AppendError("$", fmt.Errorf("to is empty"))
	}
	paramErrs.CheckAndThrowApiParameterErrors()

	type PerformanceResult struct {
		Metric interface{}     `json:"metric"`
		Values [][]interface{} `json:"values"`
	}
	type PerformanceData struct {
		ResultType string              `json:"resultType"`
		Result     []PerformanceResult `json:"result"`
	}
	type PerformanceInfo struct {
		Status string          `json:"status"`
		Data   PerformanceData `json:"data"`
	}
	type TimeResult struct {
		Metric interface{}   `json:"metric"`
		Values []interface{} `json:"value"`
	}
	type TimeData struct {
		ResultType string       `json:"resultType"`
		Result     []TimeResult `json:"result"`
	}
	type TimeInfo struct {
		Status string   `json:"status"`
		Data   TimeData `json:"data"`
	}

	//cluster没有主机时，返回空数组
	id, _ := strconv.Atoi(clusterId)
	relList, _ := model.DeployClusterHostRel.GetClusterHostRelList(id)
	if len(relList) == 0 {
		return map[string]interface{}{
			"counts": 0,
			"lists":  []map[string]interface{}{},
		}
	}

	//向Grafana请求数据

	url := fmt.Sprintf("http://%v/api/datasources/proxy/1/api/v1/query?query=prometheus_tsdb_lowest_timestamp", grafana.GrafanaURL.Host)
	res, _ := http.Get(url)
	defer res.Body.Close()
	body, _ := ioutil.ReadAll(res.Body)

	//解析json
	timeInfo := TimeInfo{}
	err = json.Unmarshal(body, &timeInfo)
	if err != nil {
		return fmt.Errorf("json unmarshal err:%v", err)
	}

	//若请求时间小于监控开始时间，同步为开始时间，防止越界
	startTime, _ := strconv.Atoi(timeInfo.Data.Result[0].Values[1].(string))
	startTime /= 1000

	if fromTime < int64(startTime) {
		fromTime = int64(startTime)
	}

	var query string
	switch metric { // 根据metric选择查询语句
	case "cpu":
		query = fmt.Sprintf("sum(100-irate(node_cpu{mode='idle',clusterId='%v',type='hosts'}[1m])*100)", clusterId)
	case "memory":
		query = fmt.Sprintf("(1-sum(node_memory_Buffers{clusterId='%v',type='hosts'}%%2Bnode_memory_Cached{clusterId='%v',type='hosts'}%%2Bnode_memory_MemFree{clusterId='%v',type='hosts'})/sum(node_memory_MemTotal{clusterId='%v',type='hosts'}))*100", clusterId, clusterId, clusterId, clusterId)
	case "disk":
		query = fmt.Sprintf("(1-sum(node_filesystem_free{clusterId='%v',type='hosts',fstype!~'rootfs|selinuxfs|autofs|rpc_pipefs|tmpfs|udev|none|devpts|sysfs|debugfs|fuse.*'})/sum(node_filesystem_size{clusterId='%v',type='hosts',fstype!~'rootfs|selinuxfs|autofs|rpc_pipefs|tmpfs|udev|none|devpts|sysfs|debugfs|fuse.*'}))*100", clusterId, clusterId)
	}

	url = fmt.Sprintf("http://%v/api/datasources/proxy/1/api/v1/query_range?query=%v&start=%v&end=%v&step=%v",
		grafana.GrafanaURL.Host, query, fromTime, toTime, (toTime-fromTime)/60) // 每次传回60个点
	res, _ = http.Get(url)
	body, _ = ioutil.ReadAll(res.Body)

	//解析json
	info := PerformanceInfo{}
	err = json.Unmarshal(body, &info)
	if err != nil {
		return fmt.Errorf("json unmarshal err:%v", err)
	}

	// 转化格式
	list := make([]map[string]interface{}, 0)
	if len(info.Data.Result) > 0 {
		for _, v := range info.Data.Result[0].Values {
			value, err := strconv.ParseFloat(v[1].(string), 64)
			if err != nil {
				return fmt.Errorf("ParseFloat err:%v", err)
			}
			list = append(list, map[string]interface{}{
				"date":  time.Unix(int64(v[0].(float64)), 0).Format("2006-01-02 15:04:05"),
				"value": value,
			})
		}
	}

	return map[string]interface{}{
		"counts": len(list),
		"lists":  list,
	}
}

func GetHostClusterAlert(ctx context.Context) apibase.Result {
	log.Debugf("HostClusterAlert: %v", ctx.Request().RequestURI)

	paramErrs := apibase.NewApiParameterErrors()
	ips := ctx.URLParam("ip")
	if ips == "" {
		paramErrs.AppendError("$", fmt.Errorf("ip is empty"))
	}
	paramErrs.CheckAndThrowApiParameterErrors()
	ipArr := strings.Split(ips, ",")
	ip := make(map[string]struct{}, len(ipArr))

	for _, k := range ipArr {
		ip[k] = struct{}{}
	}

	type HostAlertInfo struct {
		PanelTitle    string `json:"panel_title"`
		AlertName     string `json:"alert_name"`
		DashboardName string `json:"dashboard_name"`
		Url           string `json:"url"`
		State         string `json:"state"`
		Time          string `json:"time"`
	}
	list := make([]HostAlertInfo, 0)
	err, dashboardResp := grafana.GetDashboardByUid("Ne_roaViz")
	if err != nil {
		log.Errorf("get host overview dashboard error: %v", err)
		return map[string]interface{}{
			"count": len(list),
			"data":  list,
		}
	}
	param := map[string]string{
		"dashboardId": strconv.Itoa(dashboardResp.Dashboard.Id),
	}
	err, alerts := grafana.GrafanaAlertsSearch(param)
	if err != nil {
		log.Errorf("grafana search alerts error: %v", err)
		return map[string]interface{}{
			"count": len(list),
			"data":  list,
		}
	}
	for _, alert := range alerts {
		panelTitle, dashboardName := RetrievePanelTitle(alert.DashboardUid, alert.PanelId)
		//no_data, paused,alerting,ok, pending
		if alert.State == "ok" || alert.State == "paused" {
			alert.NewStateDate = ""
		}
		if alert.State != "alerting" || alert.EvalData.EvalMatches == nil {
			alert := HostAlertInfo{
				PanelTitle:    panelTitle,
				State:         alert.State,
				AlertName:     alert.Name,
				DashboardName: dashboardName,
				Url:           alert.Url,
				Time:          alert.NewStateDate,
			}
			list = append(list, alert)
		} else if alert.EvalData.EvalMatches != nil {
			exist := false
			for _, match := range alert.EvalData.EvalMatches {
				if instance, ok := match.Tags["instance"]; ok {
					if _, oks := ip[strings.Split(instance, ":")[0]]; oks && !exist {
						alert := HostAlertInfo{
							PanelTitle:    panelTitle,
							State:         alert.State,
							AlertName:     alert.Name,
							DashboardName: dashboardName,
							Url:           alert.Url,
							Time:          alert.NewStateDate,
						}
						list = append(list, alert)
						exist = true
					}
				} else {
					alert := HostAlertInfo{
						PanelTitle:    panelTitle,
						State:         alert.State,
						AlertName:     alert.Name,
						DashboardName: dashboardName,
						Url:           alert.Url,
						Time:          alert.NewStateDate,
					}
					list = append(list, alert)
				}
			}
			if !exist {
				alert := HostAlertInfo{
					PanelTitle:    panelTitle,
					State:         "ok",
					AlertName:     alert.Name,
					DashboardName: dashboardName,
					Url:           alert.Url,
					Time:          "",
				}
				list = append(list, alert)
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].State == list[j].State {
			return list[i].Time > list[j].Time
		}
		return list[i].State < list[j].State
	})
	return map[string]interface{}{
		"count": len(list),
		"data":  list,
	}
}

func GetHostGroups(ctx context.Context) apibase.Result {
	log.Debugf("HostGroups: %v", ctx.Request().RequestURI)

	var err error
	var values []interface{}

	paramErrs := apibase.NewApiParameterErrors()

	ctype := ctx.URLParam("type")
	cid := ctx.URLParam("id")

	if ctype == "" || cid == "" {
		paramErrs.AppendError("$", fmt.Errorf("cluster type or cluster id is empty"))
	}
	paramErrs.CheckAndThrowApiParameterErrors()
	values = append(values, ctype, cid)

	id, _ := strconv.Atoi(cid)
	clusterInfo, _ := model.DeployClusterList.GetClusterInfoById(id)
	var query string

	parentProductName := ctx.URLParam("parent_product_name")

	if strconv.Itoa(clusterInfo.Mode) == host.KUBERNETES_MODE {
		query = `SELECT DISTINCT deploy_node.group FROM deploy_node
LEFT JOIN deploy_cluster_host_rel ON deploy_node.sid = deploy_cluster_host_rel.sid
LEFT JOIN deploy_instance_list ON deploy_node.sid = deploy_instance_list.sid
LEFT JOIN deploy_product_list ON deploy_instance_list.pid = deploy_product_list.id
LEFT JOIN deploy_cluster_list ON deploy_cluster_host_rel.clusterId = deploy_cluster_list.id
WHERE deploy_node.isDeleted = 0 AND deploy_node.sid != '' AND deploy_cluster_host_rel.is_deleted=0 AND (deploy_cluster_list.type = ? AND deploy_cluster_list.id = ?)`
	} else {
		query = `SELECT DISTINCT deploy_host.group FROM deploy_host
LEFT JOIN deploy_cluster_host_rel ON deploy_host.sid = deploy_cluster_host_rel.sid
LEFT JOIN deploy_instance_list ON deploy_host.sid = deploy_instance_list.sid
LEFT JOIN deploy_product_list ON deploy_instance_list.pid = deploy_product_list.id
LEFT JOIN deploy_cluster_list ON deploy_cluster_host_rel.clusterId = deploy_cluster_list.id
WHERE deploy_host.isDeleted = 0 AND deploy_host.sid != '' AND deploy_cluster_host_rel.is_deleted=0 AND (deploy_cluster_list.type = ? AND deploy_cluster_list.id = ?)`
	}

	//产品名筛选
	if parentProductName != "" {
		query += ` AND deploy_product_list.parent_product_name=?`
		values = append(values, parentProductName)
	}
	if strconv.Itoa(clusterInfo.Mode) == host.KUBERNETES_MODE {
		query += ` GROUP BY deploy_node.sid`
	} else {
		query += ` GROUP BY deploy_host.sid`
	}

	groups := make([]string, 0)
	if err = model.USE_MYSQL_DB().Select(&groups, query, values...); err != nil {
		log.Errorf("HostGroups query: %v, values %v, err: %v", query, values, err)
		apibase.ThrowDBModelError(err)
	}
	return groups
}

func GetClusterList(ctx context.Context) apibase.Result {
	clusterType := ctx.URLParam("type")
	userId, err := ctx.Values().GetInt("userId")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	err, userInfo := model.UserList.GetInfoByUserId(userId)
	if err != nil {
		log.Errorf("GetInfoByUserId %v", err)
		return err
	}

	list := make([]map[string]interface{}, 0)
	clusterList := make([]model.ClusterInfo, 0)
	if userInfo.RoleId == model.ROLE_ADMIN_ID {
		clusterList, err = model.DeployClusterList.GetClusterList()
		if err != nil {
			return fmt.Errorf("Datebase err: %v", err)
		}
	} else {
		clusterList, err = model.DeployClusterList.GetClusterListByUserId(userId)
		if err != nil {
			return fmt.Errorf("Database err: %v", err)
		}
	}

	for _, cluster := range clusterList {
		if clusterType != cluster.Type && clusterType != "" {
			continue
		}
		var query string
		// 导入k8s集群左连deploy_node
		if cluster.Type == model.DEPLOY_CLUSTER_TYPE_KUBERNETES && strconv.Itoa(cluster.Mode) == host.KUBERNETES_MODE {
			query = "SELECT mem_size,mem_usage,disk_usage,cpu_cores,cpu_usage " +
				"FROM deploy_cluster_host_rel " +
				"LEFT JOIN deploy_node ON deploy_cluster_host_rel.sid = deploy_node.sid " +
				"LEFT JOIN sidecar_list ON sidecar_list.id = deploy_node.sid " +
				"WHERE deploy_cluster_host_rel.clusterId = ? and deploy_cluster_host_rel.is_deleted = 0 and mem_size is not null"
		} else {
			query = "SELECT mem_size,mem_usage,disk_usage,cpu_cores,cpu_usage " +
				"FROM deploy_cluster_host_rel " +
				"LEFT JOIN deploy_host ON deploy_cluster_host_rel.sid = deploy_host.sid " +
				"LEFT JOIN sidecar_list ON sidecar_list.id = deploy_host.sid " +
				"WHERE deploy_cluster_host_rel.clusterId = ? and deploy_cluster_host_rel.is_deleted = 0 and mem_size is not null"
		}

		if _, ok := model.DeployClusterStatus[cluster.Status]; !ok {
			log.Errorf("wrong status: %v", cluster.Status)
		}

		clusterInfo := map[string]interface{}{
			"id":          cluster.Id,
			"name":        cluster.Name,
			"type":        cluster.Type,
			"desc":        cluster.Desc,
			"version":     cluster.Version,
			"mode":        cluster.Mode,
			"tags":        cluster.Tags,
			"status":      model.DeployClusterStatus[cluster.Status],
			"create_time": cluster.CreateTime.Time,
			"create_user": cluster.CreateUser,
			"update_time": cluster.UpdateTime.Time,
			"update_user": cluster.UpdateUser,
		}

		// 如果为主机集群
		if cluster.Type == "hosts" {

			//获取主机cpu、mem、disk等数据
			type HostInfo struct {
				MemSize   int64          `db:"mem_size"`
				MemUsage  int64          `db:"mem_usage"`
				DiskUsage sql.NullString `db:"disk_usage"`
				CpuCores  int            `db:"cpu_cores"`
				CpuUsage  float64        `db:"cpu_usage"`
			}
			hostList := make([]HostInfo, 0)
			if err := model.USE_MYSQL_DB().Select(&hostList, query, cluster.Id); err != nil {
				return fmt.Errorf("Database err: %v", err)
			}

			var cpuUsage float64
			var cpuSize int
			var memSize, memUsage, diskSize, diskUsed int64

			// 累加数值
			for i := range hostList {

				var diskUsages []model.DiskUsage // 累加计算disk
				if hostList[i].DiskUsage.Valid {
					if err := json.Unmarshal([]byte(hostList[i].DiskUsage.String), &diskUsages); err != nil {
						log.Errorf("Unmarshal %v err:%v", hostList[i].DiskUsage.String, err)
					}
					for _, diskUsage := range diskUsages {
						if diskUsage.MountPoint != "/" {
							// include fileSize/Used
							diskSize += int64(diskUsage.TotalSpace)
							diskUsed += int64(diskUsage.UsedSpace)
						} else {
							diskSize += int64(diskUsage.TotalSpace)
							diskUsed += int64(diskUsage.UsedSpace)
						}
					}
				}
				memSize += hostList[i].MemSize
				memUsage += hostList[i].MemUsage
				cpuSize += hostList[i].CpuCores
				cpuUsage += hostList[i].CpuUsage * float64(hostList[i].CpuCores) / 100
			}

			memSizeDisplay, memUsedDisplay := MultiSizeConvert(memSize, memUsage)
			diskSizeDisplay, diskUsedDisplay := MultiSizeConvert(diskSize, diskUsed)

			clusterInfo["nodes"] = len(hostList)
			clusterInfo["mem_size_display"] = memSizeDisplay
			clusterInfo["mem_used_display"] = memUsedDisplay
			clusterInfo["disk_size_display"] = diskSizeDisplay
			clusterInfo["disk_used_display"] = diskUsedDisplay
			clusterInfo["cpu_core_size_display"] = strconv.Itoa(cpuSize) + "core"
			clusterInfo["cpu_core_used_display"] = strconv.FormatFloat(cpuUsage, 'f', 2, 64) + "core"

		} else {
			// 如果为k8s集群
			var allocated response.AllocatedResponse
			var content apibase.ApiResult
			sid, _ := model.DeployNodeList.GetDeployNodeSidByClusterIdAndMode(cluster.Id, cluster.Mode)
			err, nodeInfo := model.DeployNodeList.GetNodeInfoBySId(sid)
			if err != nil || time.Now().Sub(time.Time(nodeInfo.UpdateDate)) > 3*time.Minute {
				log.Infof("agent not install or wrong ")
			} else {
				// 从easykube获取所需k8s数据
				params := agent.ExecRestParams{
					Method:  "GET",
					Path:    "clientgo/allocated",
					Timeout: "5s",
				}
				resp, err := agent.AgentClient.ToExecRest(sid, &params, "")
				log.Infof("ExecRest Allocated Response:%v", resp)
				if err != nil {
					log.Errorf("ToExecRest allocated err:%v", err)
				} else {
					decodeResp, err := base64.URLEncoding.DecodeString(resp)
					if err != nil {
						log.Errorf("client-go response decode err:%v", err)
					}
					_ = json.Unmarshal(decodeResp, &content)
					data, _ := json.Marshal(content.Data)
					_ = json.Unmarshal(data, &allocated)
				}

			}

			clusterInfo["nodes"] = allocated.Nodes
			clusterInfo["pod_size_display"] = strconv.Itoa(int(allocated.PodSizeDisplay)) + "个"
			clusterInfo["pod_used_display"] = strconv.Itoa(allocated.PodUsedDisplay) + "个"
			clusterInfo["mem_size_display"] = allocated.MemSizeDisplay
			clusterInfo["mem_used_display"] = allocated.MemUsedDisplay
			clusterInfo["cpu_core_size_display"] = allocated.CpuSizeDisplay
			clusterInfo["cpu_core_used_display"] = allocated.CpuUsedDisplay
		}

		list = append(list, clusterInfo)
	}

	return map[string]interface{}{
		"counts":   len(list),
		"clusters": list,
	}
}

func GetRkeTemplate(ctx context.Context) apibase.Result {
	log.Debugf("GetRkeTemplate: %v", ctx.Request().RequestURI)
	paramErrs := apibase.NewApiParameterErrors()

	version := ctx.URLParam("version")
	clusterName := ctx.URLParam("cluster")
	networkPlugin := ctx.URLParam("network_plugin")

	if version == "" || networkPlugin == "" || clusterName == "" {
		paramErrs.AppendError("$", fmt.Errorf("cluster name or version or network_plugin is empty"))
	}
	paramErrs.CheckAndThrowApiParameterErrors()

	config := xke_service.GetDefaultRKEconfig(version, clusterName, networkPlugin)

	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	return string(data)
}

// k8s集群crud

func CreateK8sCluster(ctx context.Context) apibase.Result {
	log.Debugf("K8sCluster: %v", ctx.Request().RequestURI)
	clusterInfoReq := &view.ClusterInfoReq{}
	err := ctx.ReadJSON(clusterInfoReq)
	if err != nil { // 读取k8s集群信息
		return fmt.Errorf("[cluster] read json %T err: %v", clusterInfoReq, err)
	}
	userId, err := ctx.Values().GetInt("userId")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	// example: v1.16.3 转为 v1.16.3-rancher1-1
	clusterInfoReq.Version, err = modelkube.DeployClusterK8sAvailable.GetRealVersion(clusterInfoReq.Version)
	if err != nil {
		return err
	}
	cluster := &modelkube.ClusterInfo{
		Name:    clusterInfoReq.Name,
		Type:    clusterInfoReq.Type,
		Mode:    clusterInfoReq.Mode,
		Version: clusterInfoReq.Version,
		Desc:    clusterInfoReq.Desc,
		Tags:    clusterInfoReq.Tags,
		Configs: sql.NullString{
			String: clusterInfoReq.NetworkPlugin.String(),
			Valid:  true,
		},
		Yaml: sql.NullString{
			String: clusterInfoReq.Yaml,
			Valid:  true,
		},
		Status:     clusterInfoReq.Status,
		ErrorMsg:   clusterInfoReq.ErrorMsg,
		CreateUser: clusterInfoReq.CreateUser,
	}
	id, err := modelkube.DeployClusterList.InsertK8sCluster(cluster)
	if err != nil {
		return err
	}
	cluster.Id = id
	typ := constant.TYPE_SELF_BUILD
	if cluster.Mode == 1 {
		typ = constant.TYPE_IMPORT_CLUSTER
	}
	info := &clustergenerator.GeneratorInfo{
		Type:        typ,
		HostIp:      ctx.Request().Host,
		ClusterInfo: cluster,
	}
	err = clustergenerator.GenerateTemplate(info)
	if err != nil {
		return err
	}

	err, userInfo := model.UserList.GetInfoByUserId(userId)
	if err != nil {
		log.Errorf("GetInfoByUserId %v", err)
		return err
	}
	//写入权限
	if userInfo.RoleId != model.ROLE_ADMIN_ID {
		err, _ := model.ClusterRightList.InsertUserClusterRight(userId, id)
		if err != nil {
			log.Errorf(err.Error())
			return fmt.Errorf("can not insert ClusterRight, err : %v", err.Error())
		}
	}

	// create log file for websocket read
	if !util.IsPathExist(kutil.BuildClusterLogName(cluster.Name, id)) {
		os.Create(kutil.BuildClusterLogName(cluster.Name, id))
	}

	defer func() {
		if err := addSafetyAuditRecord(ctx, "集群管理", "创建集群", "集群名称："+cluster.Name); err != nil {
			log.Errorf("failed to add safety audit record\n")
		}
	}()

	return map[string]interface{}{
		"id":             id,
		"name":           cluster.Name,
		"desc":           cluster.Desc,
		"tags":           cluster.Tags,
		"mode":           cluster.Mode,
		"version":        cluster.Version,
		"network_plugin": clusterInfoReq.NetworkPlugin,
		"yaml":           cluster.Yaml,
	}
}

func DeleteK8sCluster(ctx context.Context) apibase.Result {
	log.Debugf("K8sCluster: %v", ctx.Request().RequestURI)

	id, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	err = model.DeployClusterList.DeleteK8sClusterById(id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}

	err = model.ClusterRightList.DeleteByClusterId(id)
	if !errors.Is(err, sql.ErrNoRows) && err != nil {
		return fmt.Errorf("Database err: %v", err)
	}

	defer func() {
		info, err := model.DeployClusterList.GetClusterInfoById(id)
		if err != nil {
			log.Errorf("%v", err)
			return
		}
		if err := addSafetyAuditRecord(ctx, "集群管理", "删除集群", "集群名称："+info.Name); err != nil {
			log.Errorf("failed to add safety audit record\n")
		}
	}()
	return nil
}

func UpdateK8sCluster(ctx context.Context) apibase.Result {
	log.Debugf("K8sCluster: %v", ctx.Request().RequestURI)

	config := model.K8sConfigInfo{}
	if err := ctx.ReadJSON(&config); err != nil { // 读取plugin信息
		return fmt.Errorf("ReadJSON err: %v", err)
	}
	jsonConfig, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("MarshalJSON err: %v", err)
	}

	cluster := model.K8sCreateInfo{}
	if err := ctx.ReadJSON(&cluster); err != nil { // 读取k8s集群信息
		return fmt.Errorf("ReadJSON2 err: %v", err)
	}
	cluster.Configs = string(jsonConfig)
	err = model.DeployClusterList.UpdateK8sCluster(cluster)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	info, err := model.DeployClusterList.GetClusterInfoById(cluster.Id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	defer func() {
		if err := addSafetyAuditRecord(ctx, "集群管理", "编辑集群", "集群名称："+cluster.Name); err != nil {
			log.Errorf("failed to add safety audit record\n")
		}
	}()
	return map[string]interface{}{
		"id":             info.Id,
		"name":           info.Name,
		"desc":           info.Desc,
		"tags":           info.Tags,
		"mode":           info.Mode,
		"version":        info.Version,
		"network_plugin": config.NetworkPlugin,
		"yaml":           info.Yaml.String,
	}
}

func GetK8sClusterInfo(ctx context.Context) apibase.Result {
	log.Debugf("K8sCluster: %v", ctx.Request().RequestURI)
	id, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	cluster, err := model.DeployClusterList.GetClusterInfoById(id)
	if err != nil {
		return fmt.Errorf("Database err: %v", err)
	}
	configs := model.K8sConfigInfo{}
	err = json.Unmarshal([]byte(cluster.Configs.String), &configs)
	if err != nil {
		return fmt.Errorf("json unmarshal err:%v", err)
	}
	return map[string]interface{}{
		"id":             cluster.Id,
		"name":           cluster.Name,
		"desc":           cluster.Desc,
		"tags":           cluster.Tags,
		"version":        strings.Split(cluster.Version, "-")[0],
		"network_plugin": configs.NetworkPlugin,
		"yaml":           cluster.Yaml.String,
	}
}

func GetK8sAvailable(ctx context.Context) apibase.Result {
	paramErrs := apibase.NewApiParameterErrors()
	mode, err := ctx.URLParamInt("mode")
	if err != nil {
		paramErrs.AppendError("$", err)
	}
	paramErrs.CheckAndThrowApiParameterErrors()

	available, err := modelkube.DeployClusterK8sAvailable.GetClusterK8sAvailableByMode(mode)
	if err != nil {
		return fmt.Errorf("Database err:%v", err)
	}
	list := make([]map[string]interface{}, 0)
	for _, v := range available {
		// 解析数据库中存的properties,分号区分配置项，冒号区分配置名，逗号区分配置选项
		//network_plugin:flannel,canal;hello_plugin:hello
		properties := strings.Split(v.Properties, ";")
		propertyList := make(map[string]interface{})
		for _, property := range properties {
			plugin := strings.Split(property, ":")
			propertyList[plugin[0]] = strings.Split(plugin[1], ",")
		}
		list = append(list, map[string]interface{}{
			"version":    strings.Split(v.Version, "-")[0],
			"properties": propertyList,
		})
	}
	return list

}

func GetK8sClusterOverView(ctx context.Context) apibase.Result {
	log.Debugf("K8sClusterOverView: %v", ctx.Request().RequestURI)
	clusterId, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}

	cluster, err := model.DeployClusterList.GetClusterInfoById(clusterId)
	if err != nil {
		return fmt.Errorf("DataBase err:%v", err)
	}

	var allocated response.AllocatedResponse
	var workload response.WorkLoadResponse
	var top5 response.Top5Response
	var content apibase.ApiResult
	var component response.ComponentResponse
	sid, err := model.DeployNodeList.GetDeployNodeSidByClusterIdAndMode(cluster.Id, cluster.Mode)
	err, nodeInfo := model.DeployNodeList.GetNodeInfoBySId(sid)
	if err != nil || time.Now().Sub(time.Time(nodeInfo.UpdateDate)) > 3*time.Minute {
		log.Infof("agent not install or wrong ")
	} else {
		// 从easykube获取所需k8s数据
		params := agent.ExecRestParams{
			Method:  "GET",
			Path:    "clientgo/workload",
			Timeout: "5s",
		}
		resp, err := agent.AgentClient.ToExecRest(sid, &params, "")
		log.Infof("ExecRest Workload Response:%v", resp)
		if err != nil {
			return fmt.Errorf("ToExecRest workload err:%v", err)
		}
		decodeResp, err := base64.URLEncoding.DecodeString(resp)
		if err != nil {
			log.Errorf("client-go response decode err:%v", err)
		}
		_ = json.Unmarshal(decodeResp, &content)
		data, _ := json.Marshal(content.Data)
		_ = json.Unmarshal(data, &workload)

		params.Path = "clientgo/top5"
		resp, err = agent.AgentClient.ToExecRest(sid, &params, "")
		log.Infof("ExecRest Top5 Response:%v", resp)
		if err != nil {
			return fmt.Errorf("ToExecRest top5 err:%v", err)
		}
		decodeResp, err = base64.URLEncoding.DecodeString(resp)
		if err != nil {
			log.Errorf("client-go response decode err:%v", err)
		}
		_ = json.Unmarshal(decodeResp, &content)
		data, _ = json.Marshal(content.Data)
		_ = json.Unmarshal(data, &top5)
		// 保留两位小数
		for i, v := range top5.MemTop5 {
			float, _ := strconv.ParseFloat(fmt.Sprintf("%.2f", v.Usage), 64)
			top5.MemTop5[i].Usage = float
		}
		for i, v := range top5.CpuTop5 {
			float, _ := strconv.ParseFloat(fmt.Sprintf("%.2f", v.Usage), 64)
			top5.CpuTop5[i].Usage = float
		}

		params.Path = "clientgo/allocated"
		resp, err = agent.AgentClient.ToExecRest(sid, &params, "")
		log.Infof("ExecRest Allocated Response:%v", resp)
		if err != nil {
			return fmt.Errorf("ToExecRest allocated err:%v", err)
		}
		decodeResp, err = base64.URLEncoding.DecodeString(resp)
		if err != nil {
			log.Errorf("client-go response decode err:%v", err)
		}
		_ = json.Unmarshal(decodeResp, &content)
		data, _ = json.Marshal(content.Data)
		_ = json.Unmarshal(data, &allocated)

		params.Path = "clientgo/componentStatus"
		resp, err = agent.AgentClient.ToExecRest(sid, &params, "")
		log.Infof("ExecRest ComponentStatus Response:%v", resp)
		if err != nil {
			return fmt.Errorf("ToExecRest componentStatus err:%v", err)
		}
		decodeResp, err = base64.URLEncoding.DecodeString(resp)
		if err != nil {
			log.Errorf("client-go response decode err:%v", err)
		}
		_ = json.Unmarshal(decodeResp, &content)
		data, _ = json.Marshal(content.Data)
		_ = json.Unmarshal(data, &component)
	}

	return map[string]interface{}{
		"mode":             cluster.Mode,
		"version":          cluster.Version,
		"create_time":      cluster.CreateTime.Time,
		"create_user":      cluster.CreateUser,
		"nodes":            allocated.Nodes,
		"error_nodes":      allocated.ErrorNodes,
		"mem_size_display": allocated.MemSizeDisplay,
		"mem_used_display": allocated.MemUsedDisplay,
		"cpu_size_display": allocated.CpuSizeDisplay,
		"cpu_used_display": allocated.CpuUsedDisplay,
		"pod_size_display": strconv.Itoa(int(allocated.PodSizeDisplay)) + "个",
		"pod_used_display": strconv.Itoa(allocated.PodUsedDisplay) + "个",
		"workload":         workload,
		"metrics":          top5,
		"component":        component.List,
	}
}

func GetK8sClusterHostList(ctx context.Context) apibase.Result {
	log.Debugf("K8sCluster: %v", ctx.Request().RequestURI)
	paramErrs := apibase.NewApiParameterErrors()
	clusterId := ctx.URLParam("cluster_id")
	if clusterId == "" {
		paramErrs.AppendError("$", fmt.Errorf("clusterId is empty"))
	}
	paramErrs.CheckAndThrowApiParameterErrors()

	group := ctx.URLParam("group")
	hostOrIp := ctx.URLParam("host_or_ip")
	isRunning := ctx.URLParam("is_running")
	status := ctx.URLParam("status")
	roles := ctx.URLParam("role")
	parentProductName := ctx.URLParam("parent_product_name")

	values := []interface{}{"%" + hostOrIp + "%", "%" + hostOrIp + "%", clusterId, 0}
	whereCause := ` AND deploy_cluster_host_rel.clusterId=? AND deploy_cluster_host_rel.is_deleted=? `

	id, _ := strconv.Atoi(clusterId)
	clusterInfo, _ := model.DeployClusterList.GetClusterInfoById(id)

	//产品名筛选
	if parentProductName != "" {
		whereCause += ` AND deploy_product_list.parent_product_name=?`
		values = append(values, parentProductName)
	}

	//主机分组筛选
	if group != "" {
		if strconv.Itoa(clusterInfo.Mode) == host.KUBERNETES_MODE {
			whereCause += ` AND deploy_node.group IN (`
		} else {
			whereCause += ` AND deploy_host.group IN (`
		}
		for i, v := range strings.Split(group, ",") {
			if i > 0 {
				whereCause += `,`
			}
			whereCause += `?`
			values = append(values, v)
		}
		whereCause += `)`
	}

	//errMssg筛选
	if status != "" {
		if strconv.Itoa(clusterInfo.Mode) == host.KUBERNETES_MODE {
			whereCause += ` AND deploy_node.errorMsg IN (`
		} else {
			whereCause += ` AND deploy_host.errorMsg IN (`
		}

		for i, v := range strings.Split(status, ",") {
			if i > 0 {
				whereCause += `,`
			}
			whereCause += `?`
			values = append(values, v)
		}
		whereCause += `)`
	}

	//role筛选
	if roles != "" {
		if strings.Contains(roles, "all") {
			whereCause += ` And deploy_cluster_host_rel.roles IN ('Etcd,Worker,Control','Etcd,Control,Worker','Worker,Control,Etcd','Worker,Etcd,Control','Control,Worker,Etcd','Control,Etcd,Worker') `
		} else {
			whereCause += ` AND (`
			for i, v := range strings.Split(roles, ",") {
				if i > 0 {
					whereCause += ` OR `
				}
				whereCause += `deploy_cluster_host_rel.roles Like '%` + v + `%'`
			}
			whereCause += `)`
		}

	}

	// 由表deploy_cluster_host_rel开始左连接
	var baseQuery string
	if strconv.Itoa(clusterInfo.Mode) == host.KUBERNETES_MODE {
		baseQuery = fmt.Sprintf(`FROM deploy_cluster_host_rel
LEFT JOIN deploy_node ON deploy_cluster_host_rel.sid = deploy_node.sid
LEFT JOIN deploy_instance_list ON deploy_node.sid = deploy_instance_list.sid
LEFT JOIN deploy_product_list ON deploy_instance_list.pid = deploy_product_list.id
LEFT JOIN sidecar_list ON sidecar_list.id = deploy_node.sid
WHERE deploy_node.sid != '' AND (deploy_node.hostname LIKE ? OR deploy_node.ip LIKE ?)%s`, whereCause)
	} else {
		baseQuery = fmt.Sprintf(`FROM deploy_cluster_host_rel
LEFT JOIN deploy_host ON deploy_cluster_host_rel.sid = deploy_host.sid
LEFT JOIN deploy_node ON deploy_host.ip = deploy_node.ip
LEFT JOIN deploy_instance_list ON deploy_node.sid = deploy_instance_list.sid
LEFT JOIN deploy_product_list ON deploy_instance_list.pid = deploy_product_list.id
LEFT JOIN sidecar_list ON sidecar_list.id = deploy_host.sid
WHERE deploy_host.sid != '' AND (deploy_host.hostname LIKE ? OR deploy_host.ip LIKE ?)%s`, whereCause)
	}

	type hostInfo struct {
		model.HostInfo
		ProductNameList        string                  `json:"product_name_list" db:"product_name_list"`
		ProductNameDisplayList string                  `json:"product_name_display_list" db:"product_name_display_list"`
		ProductIdList          string                  `json:"pid_list" db:"pid_list"`
		MemSize                int64                   `json:"mem_size" db:"mem_size"`
		MemUsage               int64                   `json:"mem_usage" db:"mem_usage"`
		CpuCores               int                     `json:"-" db:"cpu_cores"`
		DiskUsage              sql.NullString          `json:"disk_usage" db:"disk_usage"`
		NetUsage               sql.NullString          `json:"net_usage" db:"net_usage"`
		MemSizeDisplay         string                  `json:"mem_size_display"`
		MemUsedDisplay         string                  `json:"mem_used_display"`
		DiskSizeDisplay        string                  `json:"disk_size_display"`
		DiskUsedDisplay        string                  `json:"disk_used_display"`
		FileSizeDisplay        string                  `json:"file_size_display"`
		FileUsedDisplay        string                  `json:"file_used_display"`
		CpuCoreSizeDisplay     string                  `json:"cpu_core_size_display"`
		CpuCoreUsedDisplay     string                  `json:"cpu_core_used_display"`
		NetUsageDisplay        []model.NetUsageDisplay `json:"net_usage_display,omitempty"`
		IsRunning              bool                    `json:"is_running"`
		CpuUsagePct            float64                 `json:"cpu_usage_pct" db:"cpu_usage_pct"`
		MemUsagePct            float64                 `json:"mem_usage_pct" db:"mem_usage_pct"`
		DiskUsagePct           float64                 `json:"disk_usage_pct" db:"disk_usage_pct"`
		PodUsedDisplay         string                  `json:"pod_used_display"`
		PodSizeDisplay         string                  `json:"pod_size_display"`
		PodUsagePct            float64                 `json:"pod_usage_pct"`
		DBRoles                string                  `json:"-" db:"roles"`
		JSONRoles              map[string]bool         `json:"roles"`
		RunUser                string                  `json:"run_user"`
	}

	var count int
	var hostsList []hostInfo
	var query string
	whoamiCmd := "#!/bin/sh\n whoami"
	if strconv.Itoa(clusterInfo.Mode) == host.KUBERNETES_MODE {
		query = "SELECT COUNT(DISTINCT deploy_node.sid) " + baseQuery
	} else {
		query = "SELECT COUNT(DISTINCT deploy_host.sid) " + baseQuery
	}

	if err := model.USE_MYSQL_DB().Get(&count, query, values...); err != nil {
		log.Errorf("AgentHosts count query: %v, values %v, err: %v", query, values, err)
		apibase.ThrowDBModelError(err)
	}
	if count > 0 {
		if strconv.Itoa(clusterInfo.Mode) == host.KUBERNETES_MODE {
			query = "SELECT deploy_node.*, " +
				"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.product_name)),'') AS product_name_list, " +
				"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.product_name_display)),'') AS product_name_display_list, " +
				"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.id)),'') AS pid_list," +
				"sidecar_list.mem_size, sidecar_list.mem_usage, sidecar_list.disk_usage, sidecar_list.net_usage, " +
				"sidecar_list.cpu_cores, sidecar_list.cpu_usage as cpu_usage_pct, sidecar_list.mem_usage/sidecar_list.mem_size as mem_usage_pct, sidecar_list.disk_usage_pct, roles " +
				baseQuery + " GROUP BY deploy_node.sid "
		} else {
			query = "SELECT deploy_host.*, " +
				"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.product_name)),'') AS product_name_list, " +
				"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.product_name_display)),'') AS product_name_display_list, " +
				"IFNULL(GROUP_CONCAT(DISTINCT(deploy_product_list.id)),'') AS pid_list," +
				"sidecar_list.mem_size, sidecar_list.mem_usage, sidecar_list.disk_usage, sidecar_list.net_usage, " +
				"sidecar_list.cpu_cores, sidecar_list.cpu_usage as cpu_usage_pct, sidecar_list.mem_usage/sidecar_list.mem_size as mem_usage_pct, sidecar_list.disk_usage_pct, roles " +
				baseQuery + " GROUP BY deploy_host.sid "
		}

		if err := model.USE_MYSQL_DB().Select(&hostsList, query, values...); err != nil {
			log.Errorf("AgentHosts query: %v, values %v, err: %v", query, values, err)
			apibase.ThrowDBModelError(err)
		}
		for i, list := range hostsList {
			if time.Now().Sub(time.Time(list.UpdateDate)) < 3*time.Minute {
				hostsList[i].IsRunning = true
			}
			hostsList[i].JSONRoles = make(map[string]bool)
			for _, v := range strings.Split(hostsList[i].DBRoles, ",") {
				hostsList[i].JSONRoles[v] = true
			}
			hostsList[i].MemSizeDisplay, hostsList[i].MemUsedDisplay = MultiSizeConvert(list.MemSize, list.MemUsage)

			hostsList[i].CpuCoreUsedDisplay = strconv.FormatFloat(list.CpuUsagePct*float64(list.CpuCores)/100, 'f', 2, 64) + "core"
			hostsList[i].CpuCoreSizeDisplay = strconv.Itoa(list.CpuCores) + "core"
			if list.DiskUsage.Valid {
				//hostsList[i].DiskSizeDisplay, hostsList[i].DiskUsedDisplay, hostsList[i].FileSizeDisplay, hostsList[i].FileUsedDisplay = diskUsageConvert(list.DiskUsage.String)
				var diskUsages []model.DiskUsage
				if err := json.Unmarshal([]byte(list.DiskUsage.String), &diskUsages); err != nil {
					return err
				}
				var diskSize, diskUsed, fileSize, fileUsed int64
				for _, diskUsage := range diskUsages {
					if diskUsage.MountPoint != "/" {
						fileSize += int64(diskUsage.TotalSpace)
						fileUsed += int64(diskUsage.UsedSpace)
						// include fileSize/Used
						diskSize += int64(diskUsage.TotalSpace)
						diskUsed += int64(diskUsage.UsedSpace)
					} else {
						diskSize += int64(diskUsage.TotalSpace)
						diskUsed += int64(diskUsage.UsedSpace)
					}
				}
				hostsList[i].DiskSizeDisplay, hostsList[i].DiskUsedDisplay = MultiSizeConvert(diskSize, diskUsed)
				hostsList[i].FileSizeDisplay, hostsList[i].FileUsedDisplay = MultiSizeConvert(fileSize, fileUsed)
			}
			if list.NetUsage.Valid {
				hostsList[i].NetUsageDisplay = netUsageConvert(list.NetUsage.String)
			}
			if list.IsDeleted == 0 && list.Status > 0 && hostsList[i].IsRunning {
				content, err := agent.AgentClient.ToExecCmdWithTimeout(list.SidecarId, "", whoamiCmd, "5s", "", "")
				if err != nil {
					//exec failed
					content = err.Error()
				}
				user := strings.Replace(content, LINUX_SYSTEM_LINES, "", -1)
				hostsList[i].RunUser = user
			}
		}
	}

	//过滤 运行中/停止状态，供共两种状态
	//数据库里没有运行中/停止状态，仅在这个做简单过滤
	result := make([]hostInfo, 0)
	ret := strings.Split(isRunning, ",")
	if len(isRunning) > 0 && len(ret) == 1 {
		for _, v := range hostsList {
			if isRunning == strconv.FormatBool(v.IsRunning) {
				result = append(result, v)
			}
		}
	} else {
		result = hostsList
	}

	// 通过client-go获取主机pod资源

	var podList response.PodListResponse
	var content apibase.ApiResult
	cluster, err := model.DeployClusterList.GetClusterInfoById(id)
	if err != nil {
		return fmt.Errorf("Database err:%v", err)
	}
	sid, err := model.DeployNodeList.GetDeployNodeSidByClusterIdAndMode(cluster.Id, cluster.Mode)
	err, nodeInfo := model.DeployNodeList.GetNodeInfoBySId(sid)
	if err != nil || time.Now().Sub(time.Time(nodeInfo.UpdateDate)) > 3*time.Minute {
		log.Infof("agent not install or wrong ")
		for i := range result {
			result[i].PodUsagePct = 0
			result[i].PodUsedDisplay = "0个"
			result[i].PodSizeDisplay = "0个"
		}
	} else {
		// 从easykube获取所需k8s数据
		params := agent.ExecRestParams{
			Method:  "GET",
			Path:    "clientgo/allocatedPodList",
			Timeout: "5s",
		}
		resp, err := agent.AgentClient.ToExecRest(sid, &params, "")
		log.Infof("ExecRest AllocatedPodList Response:%v", resp)
		if err != nil {
			log.Errorf("ToExecRest podList err:%v", err)
			for i := range result {
				result[i].PodUsagePct = 0
				result[i].PodUsedDisplay = "0个"
				result[i].PodSizeDisplay = "0个"
			}
		} else {
			decodeResp, err := base64.URLEncoding.DecodeString(resp)
			if err != nil {
				log.Errorf("client-go response decode err:%v", err)
			}
			_ = json.Unmarshal(decodeResp, &content)
			data, _ := json.Marshal(content.Data)
			_ = json.Unmarshal(data, &podList)

			podSet := make(map[string]response.NodePod)
			for _, nodePod := range podList.List {
				podSet[nodePod.LocalIp] = nodePod
			}
			for i := range result {
				result[i].PodUsagePct = podSet[result[i].Ip].PodUsagePct
				result[i].PodUsedDisplay = strconv.Itoa(podSet[result[i].Ip].PodUsed) + "个"
				result[i].PodSizeDisplay = strconv.Itoa(int(podSet[result[i].Ip].PodSize)) + "个"
			}
		}
	}

	// 重写排序
	pagination := apibase.GetPaginationFromQueryParameters(nil, ctx, nil)
	switch pagination.SortBy {
	case "pod_usage_pct":
		sort.SliceStable(result, func(i, j int) bool {
			if pagination.SortDesc {
				return result[i].PodUsagePct > result[j].PodUsagePct
			} else {
				return result[i].PodUsagePct < result[j].PodUsagePct
			}
		})
	case "cpu_usage_pct":
		sort.SliceStable(result, func(i, j int) bool {
			if pagination.SortDesc {
				return result[i].CpuUsagePct > result[j].CpuUsagePct
			} else {
				return result[i].CpuUsagePct < result[j].CpuUsagePct
			}
		})
	case "mem_usage_pct":
		sort.SliceStable(result, func(i, j int) bool {
			if pagination.SortDesc {
				return result[i].MemUsagePct > result[j].MemUsagePct
			} else {
				return result[i].MemUsagePct < result[j].MemUsagePct
			}
		})
	}
	// 重写分页
	total := len(result) // result总数量
	if pagination.Start > 0 {
		if pagination.Start+pagination.Limit < total {
			result = result[pagination.Start : pagination.Start+pagination.Limit]
		} else if pagination.Start > total {
			result = nil
		} else {
			result = result[pagination.Start:total]
		}
	} else {
		if pagination.Limit == 0 {
			result = result[:total]
		} else if pagination.Limit < total {
			result = result[:pagination.Limit]
		}
	}

	return map[string]interface{}{
		"hosts": result,
		"count": count,
	}
}

func GetK8sClusterImportCmd(ctx context.Context) apibase.Result {
	log.Debugf("GetK8sClusterImportCmd: %v", ctx.Request().RequestURI)
	clusterId := ctx.URLParam("cluster_id")
	clusterName := ctx.URLParam("cluster_name")
	if clusterId == "" {
		return fmt.Errorf("[cluster]: clusterid is empty")
	}
	cid, err := strconv.Atoi(clusterId)
	if err != nil {
		log.Errorf("[cluster]: clusterid is not a number %s, error: %v", clusterId, err)
		return err
	}
	secure := "kubectl apply -f %s"
	inSecure := "curl --insecure -sfL %s | kubectl apply -f -"
	info := clustergenerator.GeneratorInfo{
		Type: constant.TYPE_IMPORT_CLUSTER,
		ClusterInfo: &modelkube.ClusterInfo{
			Id:   cid,
			Name: clusterName,
		},
	}
	url, err := clustergenerator.GetTemplateUrl(&info, false)
	if err != nil {
		return err
	}
	secure = fmt.Sprintf(secure, url)
	inSecure = fmt.Sprintf(inSecure, url)

	url2, err := clustergenerator.GetTemplateUrl(&info, true)
	if err != nil {
		return err
	}
	secureV1beta1 := fmt.Sprintf("kubectl apply -f %s", url2)
	insecureV1beta1 := fmt.Sprintf("curl --insecure -sfL %s | kubectl apply -f -", url2)
	return map[string]interface{}{
		"secure":           secure,
		"secure_v1beta1":   secureV1beta1,
		"insecure":         inSecure,
		"insecure_v1beta1": insecureV1beta1,
	}
}

func GetK8sClusterPerformance(ctx context.Context) apibase.Result {
	log.Debugf("HostClusterPerformance: %v", ctx.Request().RequestURI)

	paramErrs := apibase.NewApiParameterErrors()
	clusterId := ctx.Params().Get("cluster_id")
	if clusterId == "" {
		paramErrs.AppendError("$", fmt.Errorf("clusterId is empty"))
	}
	metric := ctx.URLParam("metric")
	if metric == "" {
		paramErrs.AppendError("$", fmt.Errorf("metric is empty"))
	}
	fromTime, err := ctx.URLParamInt64("from")
	if err != nil {
		paramErrs.AppendError("$", fmt.Errorf("from is empty"))
	}
	toTime, err := ctx.URLParamInt64("to")
	if err != nil {
		paramErrs.AppendError("$", fmt.Errorf("to is empty"))
	}
	paramErrs.CheckAndThrowApiParameterErrors()

	type PerformanceResult struct {
		Metric interface{}     `json:"metric"`
		Values [][]interface{} `json:"values"`
	}
	type PerformanceData struct {
		ResultType string              `json:"resultType"`
		Result     []PerformanceResult `json:"result"`
	}
	type PerformanceInfo struct {
		Status string          `json:"status"`
		Data   PerformanceData `json:"data"`
	}
	type TimeResult struct {
		Metric interface{}   `json:"metric"`
		Values []interface{} `json:"value"`
	}
	type TimeData struct {
		ResultType string       `json:"resultType"`
		Result     []TimeResult `json:"result"`
	}
	type TimeInfo struct {
		Status string   `json:"status"`
		Data   TimeData `json:"data"`
	}

	//cluster没有主机时，返回空数组
	id, _ := strconv.Atoi(clusterId)
	relList, _ := model.DeployClusterHostRel.GetClusterHostRelList(id)
	if len(relList) == 0 {
		return map[string]interface{}{
			"counts": 0,
			"lists":  []map[string]interface{}{},
		}
	}

	// 不支持获得pod数据
	if metric == "pod" {
		return map[string]interface{}{
			"counts": 0,
			"lists":  make([]map[string]interface{}, 0),
		}
	}

	//向Grafana请求数据

	url := fmt.Sprintf("http://%v/api/datasources/proxy/1/api/v1/query?query=prometheus_tsdb_lowest_timestamp", grafana.GrafanaURL.Host)
	res, _ := http.Get(url)
	defer res.Body.Close()
	body, _ := ioutil.ReadAll(res.Body)

	//解析json
	timeInfo := TimeInfo{}
	err = json.Unmarshal(body, &timeInfo)
	if err != nil {
		log.Errorf("json unmarshal err:%v", err)
	}

	//若请求时间小于监控开始时间，同步为开始时间，防止越界
	startTime, _ := strconv.Atoi(timeInfo.Data.Result[0].Values[1].(string))
	startTime /= 1000

	if fromTime < int64(startTime) {
		fromTime = int64(startTime)
	}

	var query string
	switch metric { // 根据metric选择查询语句
	case "cpu":
		query = fmt.Sprintf("sum(100-irate(node_cpu{mode='idle',clusterId='%v',type='kubernetes'}[3m])*100)", clusterId)
	case "memory":
		query = fmt.Sprintf("(1-sum(node_memory_Buffers{clusterId='%v',type='kubernetes'}%%2Bnode_memory_Cached{clusterId='%v',type='kubernetes'}%%2Bnode_memory_MemFree{clusterId='%v',type='kubernetes'})/sum(node_memory_MemTotal{clusterId='%v',type='kubernetes'}))*100", clusterId, clusterId, clusterId, clusterId)
	case "disk":
		query = fmt.Sprintf("(1-sum(node_filesystem_free{clusterId='%v',type='kubernetes',fstype!~'rootfs|selinuxfs|autofs|rpc_pipefs|tmpfs|udev|none|devpts|sysfs|debugfs|fuse.*'})/sum(node_filesystem_size{clusterId='%v',type='kubernetes',fstype!~'rootfs|selinuxfs|autofs|rpc_pipefs|tmpfs|udev|none|devpts|sysfs|debugfs|fuse.*'}))*100", clusterId, clusterId)
	}

	url = fmt.Sprintf("http://%v/api/datasources/proxy/1/api/v1/query_range?query=%v&start=%v&end=%v&step=%v",
		grafana.GrafanaURL.Host, query, fromTime, toTime, (toTime-fromTime)/60) // 每次传回60个点
	res, _ = http.Get(url)
	body, _ = ioutil.ReadAll(res.Body)

	//解析json
	info := PerformanceInfo{}
	err = json.Unmarshal(body, &info)
	if err != nil {
		log.Errorf("json unmarshal err:%v", err)
	}

	// 转化格式
	list := make([]map[string]interface{}, 0)
	if len(info.Data.Result) > 0 {
		for _, v := range info.Data.Result[0].Values {
			value, err := strconv.ParseFloat(v[1].(string), 64)
			if err != nil {
				log.Errorf("ParseFloat err:%v", err)
			}
			list = append(list, map[string]interface{}{
				"date":  time.Unix(int64(v[0].(float64)), 0).Format("2006-01-02 15:04:05"),
				"value": value,
			})
		}
	}

	return map[string]interface{}{
		"counts": len(list),
		"lists":  list,
	}
}

func MultiSizeConvert(size1, size2 int64) (string, string) {
	sizeUnits := [...]string{"B", "KB", "MB", "GB", "TB"}
	f1 := float32(size1)
	f2 := float32(size2)
	for _, v := range sizeUnits {
		if f1 < 1024 && f2 < 1024 {
			return fmt.Sprintf("%.2f"+v, f1), fmt.Sprintf("%.2f"+v, f2)
		} else {
			f1 = f1 / 1024
			f2 = f2 / 1024
		}
	}
	return fmt.Sprintf("%.2f"+sizeUnits[len(sizeUnits)-1], f1), fmt.Sprintf("%.2f"+sizeUnits[len(sizeUnits)-1], f1)
}

func GetClusterProductList(ctx context.Context) apibase.Result {
	log.Debugf("GetClusterProductList: %v", ctx.Request().RequestURI)
	userId, err := ctx.Values().GetInt("userId")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	err, userInfo := model.UserList.GetInfoByUserId(userId)
	if err != nil {
		log.Errorf("GetInfoByUserId %v", err)
		return err
	}
	// 获取所有running的集群
	clusterList := make([]model.ClusterInfo, 0)
	if userInfo.RoleId == model.ROLE_ADMIN_ID {
		clusterList, err = model.DeployClusterList.GetDeployedClusterList()
		if err != nil {
			return fmt.Errorf("[GetClusterProductList] Get deploy cluster list err:%v", err)
		}
	} else {
		clusterList, err = model.DeployClusterList.GetDeployedClusterListByUserId(userId)
		if err != nil {
			return fmt.Errorf("[GetDeployedClusterListByUserId] Get deploy cluster list err:%v", err)
		}
	}
	list := make([]map[string]interface{}, 0)

	for _, cluster := range clusterList {
		// 生成主机模式下的集群产品包信息
		if cluster.Type == "hosts" {
			parentProductNames, err := model.DeployClusterProductRel.GetParentProductNameListByClusterIdNamespace(cluster.Id, "")
			if err != nil {
				return fmt.Errorf("[GetClusterProductList] Get parentProductName list with clusterid err:%v", err)
			}
			products := make([]string, 0)
			subdomain := make(map[string]interface{}, 0)
			for _, name := range parentProductNames {
				products = append(products, name)
			}
			subdomain["products"] = products
			list = append(list, map[string]interface{}{
				"clusterName": cluster.Name,
				"clusterId":   cluster.Id,
				"clusterType": cluster.Type,
				"mode":        cluster.Mode,
				"subdomain":   subdomain,
			})
		} else if cluster.Type == "kubernetes" {
			// 获取指定k8s集群下的namespace列表
			cluster_nslist, err := modelkube.DeployNamespaceList.GetLike("", cluster.Id, constant.NAMESPACE_VALID, true)
			if err != nil {
				return fmt.Errorf("[GetClusterProductList] Get namespace list with clusterid err:%v", err)
			}

			subdomain := make(map[string]interface{}, 0)
			// 获取namespace下部署过的父级产品包名称
			for _, v := range cluster_nslist {
				parentProductNames, err := model.DeployClusterProductRel.GetParentProductNameListByClusterIdNamespace(cluster.Id, v.Namespace)
				if err != nil {
					return fmt.Errorf("[GetClusterProductList] Get parentProductName list with clusterid and namespace err:%v", err)
				}

				products := make([]string, 0)
				// 防止没有部署应用的namespace在前端展示
				if len(parentProductNames) == 0 {
					continue
				}
				for _, name := range parentProductNames {
					products = append(products, name)
				}
				subdomain[v.Namespace] = products
			}
			// 防止新增加的k8s集群在没有部署应用情况下在前端展示
			if len(subdomain) == 0 {
				continue
			}
			list = append(list, map[string]interface{}{
				"clusterName": cluster.Name,
				"clusterId":   cluster.Id,
				"clusterType": cluster.Type,
				"mode":        cluster.Mode,
				"subdomain":   subdomain,
			})
		}
	}
	return list
}

func GetK8sClusterNameSpaceList(ctx context.Context) apibase.Result {
	log.Debugf("GetK8sClusterNameSpaceList: %v", ctx.Request().RequestURI)

	clusterId, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	cluster, err := model.DeployClusterList.GetClusterInfoById(clusterId)
	if err != nil {
		return err
	}
	if cluster.Type == "kubernetes" && cluster.Mode == 1 {
		rsp, err := resource.NamespaceList(&cluster)
		if err != nil {
			return err
		}
		return rsp
	}

	//the original way
	content := oldclient.ContentResponse{}
	namespaces := oldclient.NamespaceListResponse{}
	clientParam := agent.ExecRestParams{
		Method:  "GET",
		Path:    "clientgo/namespace/list",
		Timeout: "5s",
	}

	sid, _ := model.DeployNodeList.GetDeployNodeSidByClusterIdAndMode(clusterId, cluster.Mode)
	resp, err := agent.AgentClient.ToExecRest(sid, &clientParam, "")
	if err != nil {
		return fmt.Errorf("ToExecRest namespace create err:%v", err)
	}

	decodeResp, err := base64.URLEncoding.DecodeString(resp)
	if err != nil {
		log.Errorf("client-go response decode err:%v", err)
	}
	_ = json.Unmarshal(decodeResp, &content)
	data, _ := json.Marshal(content.Data)
	_ = json.Unmarshal(data, &namespaces)

	filter := []oldclient.Namespace{}
	for _, namespace := range namespaces.Namespaces {
		if strings.HasPrefix(namespace.Name, kmodel.NAMESPACE_PREFIX) {
			filter = append(filter, namespace)
		}
	}

	return map[string]interface{}{
		"clusterName": cluster.Name,
		"clusterId":   cluster.Id,
		"clusterType": cluster.Type,
		"namespaces":  filter,
	}
}

func CreateK8sClusterNamespace(ctx context.Context) apibase.Result {
	log.Debugf("CreateK8sClusterNamespace: %v", ctx.Request().RequestURI)

	clusterId, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	cluster, err := model.DeployClusterList.GetClusterInfoById(clusterId)
	if err != nil {
		return err
	}

	param := make(map[string]interface{})
	if err := ctx.ReadJSON(&param); err != nil {
		return fmt.Errorf("ReadJSON err: %v", err)
	}

	body, _ := json.Marshal(param)
	content := oldclient.ContentResponse{}
	namespace := oldclient.Namespace{}
	clientParam := agent.ExecRestParams{
		Method:  "POST",
		Path:    "clientgo/namespace/create",
		Body:    body,
		Timeout: "5s",
	}

	sid, _ := model.DeployNodeList.GetDeployNodeSidByClusterIdAndMode(clusterId, cluster.Mode)
	resp, err := agent.AgentClient.ToExecRest(sid, &clientParam, "")
	if err != nil {
		return fmt.Errorf("ToExecRest namespace create err:%v", err)
	}

	decodeResp, err := base64.URLEncoding.DecodeString(resp)
	if err != nil {
		log.Errorf("client-go response decode err:%v", err)
	}
	_ = json.Unmarshal(decodeResp, &content)
	if content.Code != 0 {
		return fmt.Errorf(content.Msg)
	}
	data, _ := json.Marshal(content.Data)
	_ = json.Unmarshal(data, &namespace)
	return namespace.Name
}

func K8sProductListWatch(ctx context.Context) apibase.Result {
	param := events.Event{}
	if err := ctx.ReadJSON(&param); err != nil {
		return fmt.Errorf("ReadJSON err: %v", err)
	}
	return monitor.HandleResourceM(&param)
}

//func GetK8sClusterProductDepends(ctx context.Context) apibase.Result {
//	log.Debugf("GetK8sClusterProductDepends: %v", ctx.Request().RequestURI)
//
//	cid, err := ctx.Params().GetInt("cluster_id")
//	if err != nil {
//		return fmt.Errorf("cluster_id is empty")
//	}
//	pid, err := ctx.Params().GetInt("pid")
//	if err != nil {
//		return fmt.Errorf("pid is empty")
//	}
//	namespace := ctx.Params().Get("namespace_name")
//	if namespace == "" {
//		return fmt.Errorf("namespace is empty")
//	}
//
//	info, err := model.DeployProductList.GetProductInfoById(pid)
//	if err != nil {
//		return fmt.Errorf("database err:%v", err)
//	}
//	originSchema, err := schema.Unmarshal(info.Schema)
//	if err != nil {
//		log.Errorf("schema unmarshal err:%v", err.Error())
//		return err
//	}
//	// 获取该部署产品包下服务组件依赖其它产品包的信息，如DTUic依赖DTBase的redis,如:{"DTBase":["redis"]}
//	baseMap := make(map[string][]string)
//	for _, config := range originSchema.Service {
//		if config.BaseProduct == "" || config.BaseAtrribute != BASE_SERVICE_BRIDGE {
//			continue
//		}
//		if _, ok := baseMap[config.BaseProduct]; ok {
//			baseMap[config.BaseProduct] = append(baseMap[config.BaseProduct], config.BaseService)
//		} else {
//			baseMap[config.BaseProduct] = []string{config.BaseService}
//		}
//	}
//
//	// 获取所有上传了的产品包的id和产品包的名称，显示格式如:{product_id:"DTBase",2:"DTUic"}
//	productMap, err := model.DeployProductList.GetProductPidAndNameMap()
//	if err != nil {
//		log.Errorf("database err:%v", err.Error())
//		return err
//	}
//	// 获取所有集群信息列表
//	clusterList, err := model.DeployClusterList.GetClusterList()
//	if err != nil {
//		log.Errorf("database err:%v", err.Error())
//		return err
//	}
//
//	candidates := make([]map[string]interface{}, 0)
//	hasDepends := false
//	if len(baseMap) > 0 {
//		hasDepends = true
//		for _, cluster := range clusterList {
//			relList, err := model.DeployClusterProductRel.GetProductListByClusterId(cluster.Id, model.PRODUCT_STATUS_DEPLOYED)
//			if err != nil {
//				log.Errorf("database err:%v", err.Error())
//				return err
//			}
//
//			// 获取集群下部署成功的产品包信息如:{"DTBase":[product_id]}
//			relMap := make(map[string][]int)
//			for _, productRel := range relList { // make deployed product map
//				if _, ok := relMap[productRel.ProductName]; ok {
//					relMap[productRel.ProductName] = append(relMap[productRel.ProductName], productRel.ID)
//				} else {
//					relMap[productRel.ProductName] = []int{productRel.ID}
//				}
//			}
//
//			isCandidate := true
//			for baseProductName, baseServiceNames := range baseMap { // check isCandidate
//				if len(relMap[baseProductName]) == 0 {
//					isCandidate = false
//					break
//				}
//				for _, pid := range relMap[baseProductName] {
//					if !isCandidate {
//						break
//					}
//					serviceNameSet, err := model.DeployInstanceList.GetServiceNameByClusterIdAndPid(cluster.Id, pid)
//					if err != nil {
//						log.Errorf("database err:%v", err.Error())
//						return err
//					}
//
//					for _, baseServiceName := range baseServiceNames {
//						if !serviceNameSet[baseServiceName] {
//							isCandidate = false
//							break
//						}
//					}
//				}
//			}
//			if isCandidate {
//				candidates = append(candidates, map[string]interface{}{
//					"clusterId":   cluster.Id,
//					"clusterName": cluster.Name,
//				})
//			}
//		}
//	}
//
//	// 检索产品包依赖的集群
//	target, err := model.DeployKubeBaseProduct.GetByPidAndClusterIdAndNamespace(pid, cid, namespace)
//	if err != nil && err != sql.ErrNoRows {
//		log.Errorf("database err:%v", err.Error())
//		return err
//	}
//
//	message := ""
//	// isTargetError=true表示若先前部署的产品包依赖集群，不存在候选依赖集群列表中，表示旧的依赖集群被移除了
//	isTargetError := true
//	for _, candidate := range candidates {
//		if target.BaseClusterId == candidate["clusterId"] {
//			isTargetError = false
//			break
//		}
//	}
//	if hasDepends && len(candidates) == 0 {
//		message = "检测到该产品依赖的组件在所有集群中不存在，请先部署依赖组件"
//		for product := range baseMap {
//			message += " " + product
//		}
//	} else if isTargetError && target.BaseClusterId != 0 {
//		baseCluster, _ := model.DeployClusterList.GetClusterInfoById(target.BaseClusterId)
//		message = "检测到该产品依赖的组件在多个集群中存在，旧依赖集群 " + baseCluster.Name + " 中的产品可能发生迁移或卸载，请手动选择一个依赖集群"
//	} else {
//		message = "检测到该产品依赖的组件在多个集群中存在，请手动选择一个依赖集群"
//	}
//
//	return map[string]interface{}{
//		"namespace":   namespace,
//		"clusterId":   cid,
//		"productName": productMap[pid],
//		"candidates":  candidates,
//		"hasDepends":  hasDepends,
//		"message":     message,
//		"targets": map[string]interface{}{
//			"clusterId": target.BaseClusterId,
//		},
//	}
//}

func GetK8sClusterProductDepends(ctx context.Context) apibase.Result {
	log.Debugf("GetK8sClusterProductDepends: %v", ctx.Request().RequestURI)

	cid, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		return fmt.Errorf("cluster_id is empty")
	}
	pid, err := ctx.Params().GetInt("pid")
	if err != nil {
		return fmt.Errorf("pid is empty")
	}
	namespace := ctx.Params().Get("namespace_name")
	if namespace == "" {
		return fmt.Errorf("namespace is empty")
	}

	info, err := model.DeployProductList.GetProductInfoById(pid)
	if err != nil {
		return fmt.Errorf("database err:%v", err)
	}
	originSchema, err := schema.Unmarshal(info.Schema)
	if err != nil {
		log.Errorf("schema unmarshal err:%v", err.Error())
		return fmt.Errorf("schema json unmarshal err:%v", err)
	}
	// 获取该部署产品包下服务组件依赖其它产品包的信息，如DTUic依赖DTBase的redis,如:{"DTBase":["redis"]}
	baseMap := make(map[string][]string)
	for _, config := range originSchema.Service {
		if config.BaseProduct == "" || config.BaseService == "" || config.BaseAtrribute == BASE_SERVICE_OPTIONAL {
			continue
		}
		if _, ok := baseMap[config.BaseProduct]; ok {
			baseMap[config.BaseProduct] = append(baseMap[config.BaseProduct], config.BaseService)
		} else {
			baseMap[config.BaseProduct] = []string{config.BaseService}
		}
	}

	// 获取当前集群下部署成功的产品包列表
	relList, err := model.DeployClusterProductRel.GetProductListByClusterId(cid, model.PRODUCT_STATUS_DEPLOYED)
	if err != nil {
		log.Errorf("[GetK8sClusterProductDepends->GetProductListByClusterId] database err:%v", err.Error())
		return fmt.Errorf("[GetK8sClusterProductDepends->GetProductListByClusterId] database err: %v", err)
	}

	// 获取namespace空间下对应部署的产品包，如{ns1:["product1","product2"]}
	relnsMap := make(map[string][]model.DeployProductListInfo)
	for _, product := range relList {
		if _, ok := relnsMap[product.Namespace]; ok {
			relnsMap[product.Namespace] = append(relnsMap[product.Namespace], product)
		} else {
			relnsMap[product.Namespace] = []model.DeployProductListInfo{product}
		}
	}

	// 候选列表
	candidates := make([]map[string]interface{}, 0)
	hasDepends := false
	if len(baseMap) > 0 {
		hasDepends = true
		for relns, relproducts := range relnsMap {
			isCandidate := false
			count := 0
			// 排除不存在需要依赖产品包的命名空间
			for _, relproduct := range relproducts {
				if _, ok := baseMap[relproduct.ProductName]; ok {
					count++
				}
			}
			// 只有符合产品包中依赖的产品包信息的namespace才能进入候选
			if count == len(baseMap) {
				isCandidate = true
				for _, relproduct := range relproducts {
					serviceNameSet, err := model.DeployInstanceList.GetServiceNameByClusterIdAndPid(cid, relproduct.ID, relproduct.Namespace)
					if err != nil {
						log.Errorf("[GetK8sClusterProductDepends->GetServiceNameByClusterIdAndPid]database err:%v", err.Error())
						return fmt.Errorf("[GetK8sClusterProductDepends->GetServiceNameByClusterIdAndPid]database err:%v", err)
					}

					// 依赖的服务是否都部署
					for _, baseServiceName := range baseMap[relproduct.ProductName] {
						if !serviceNameSet[baseServiceName] {
							isCandidate = false
							break
						}
					}
				}
			}

			if isCandidate {
				candidates = append(candidates, map[string]interface{}{
					"relynamespace": relns,
				})
			}
		}
	}

	// 检索产品包依赖的集群记录
	targetrecord, err := model.DeployKubeBaseProduct.GetByPidAndClusterIdAndNamespace(pid, cid, namespace)
	if err != nil && err != sql.ErrNoRows {
		log.Errorf("[GetK8sClusterProductDepends->GetByPidAndClusterIdAndNamespace] database err:%v", err.Error())
		return fmt.Errorf("[GetK8sClusterProductDepends->GetByPidAndClusterIdAndNamespace] database err:%v", err)
	}

	message := ""

	// isTargetNotExist=true表示先前部署的产品包依赖所依赖的namespace不存在候选依赖namespace列表中，表示旧的依赖namespace被移除了
	isTargetNotExist := true
	for _, candidate := range candidates {
		if targetrecord.RelyNamespace == candidate["relynamespace"] {
			isTargetNotExist = false
			break
		}
	}

	if hasDepends && len(candidates) == 0 {
		message = "检测到该产品依赖的组件在当前集群中不存，请先部署依赖组件"
		for product := range baseMap {
			message += " " + product
		}
	} else if isTargetNotExist && targetrecord.RelyNamespace != "" {
		message = "检测到该产品依赖的组件在当前集群中存在，旧依赖namespace " + targetrecord.RelyNamespace + " 中的产品可能发生迁移或卸载，请手动选择一个依赖namespace"
		targetrecord.RelyNamespace = ""
	} else if hasDepends && len(candidates) > 0 {
		message = "检测到该产品依赖的组件在当前集群下存在，请手动选择一个依赖的namespace"
	} else {
		message = "检测到该产品不存在依赖的组件或者依赖方式为option"
	}
	// 判断当前部署的namespace是否存在依赖的产品包，若存在则默认选择当前namespace
	if targetrecord.RelyNamespace == "" {
		for _, candidate := range candidates {
			if namespace == candidate["relynamespace"] {
				targetrecord.RelyNamespace = namespace
			}
		}
	}

	return map[string]interface{}{
		"namespace":   namespace,
		"clusterId":   cid,
		"productName": info.ProductName,
		"candidates":  candidates,
		"hasDepends":  hasDepends,
		"message":     message,
		"targets": map[string]interface{}{
			"relynamespace": targetrecord.RelyNamespace,
		},
	}
}

type k8sDeployParam struct {
	UncheckedServices []string `json:"unchecked_services,omitempty"`
	ClusterId         int      `json:"clusterId"`
	Pid               int      `json:"pid"`
	Namespace         string   `json:"namespace,omitempty"`
	RelyNamespace     string   `json:"relynamespace,omitempty"`
}

func DeployK8sProduct(ctx context.Context) apibase.Result {
	log.Debugf("DeployK8sProduct: %v", ctx.Request().RequestURI)
	productName := ctx.Params().Get("product_name")
	productVersion := ctx.Params().Get("product_version")
	if productName == "" || productVersion == "" {
		return fmt.Errorf("product_name or product_version is empty")
	}
	userId, err := ctx.Values().GetInt("userId")
	if err != nil {
		return fmt.Errorf("get userId err: %v", err)
	}
	param := k8sDeployParam{}
	if err = ctx.ReadJSON(&param); err != nil {
		return fmt.Errorf("ReadJSON err: %v", err)
	}
	if param.ClusterId == 0 {
		param.ClusterId, err = GetCurrentClusterId(ctx)
		if err != nil {
			log.Errorf("%v", err)
			return err
		}
	}
	log.Infof("deploy product_name:%v, product_version: %v, userId: %v, clusterId: %v", productName, productVersion, userId, param.ClusterId)
	defer func() {
		info, err := model.DeployClusterList.GetClusterInfoById(param.ClusterId)
		if err != nil {
			log.Errorf("%v\n", err)
			return
		}
		if err := addSafetyAuditRecord(ctx, "部署向导", "产品部署", "集群名称："+info.Name+", "+productName+productVersion); err != nil {
			log.Errorf("failed to add safety audit record\n")
		}
	}()
	return DealK8SDeploy(param.Namespace, param.UncheckedServices, userId, param.ClusterId, param.RelyNamespace, param.Pid)
}

// get current product's baseservice config and sets as the baseservice's config
// clusterId: the current cluster
// baseClusterId: the relied host type cluster
// sc: the current product
func inheritK8sBaseService(clusterId int, relyNamespace string, sc *schema.SchemaConfig, s sqlxer) error {
	var err error
	for _, name := range sc.GetBaseService() {
		baseProduct := sc.Service[name].BaseProduct
		//baseProductVersion := sc.Service[name].BaseProductVersion
		baseService := sc.Service[name].BaseService
		baseAtrri := sc.Service[name].BaseAtrribute
		baseConfigMap, ips, hosts, version, bsvc, err_ := getBaseServicInfo(s, baseProduct, baseService, baseAtrri, relyNamespace, clusterId)
		if err_ != nil {
			err = errors2.Wrap(err, fmt.Errorf("get base service %v(BaseProduct:%v,  BaseService:%v) error:%v", name, baseProduct, baseService, err_))
			continue
		}
		err_ = sc.SetBaseService(name, baseConfigMap, ips, hosts, version)
		if err_ != nil {
			err = errors2.Wrap(err, fmt.Errorf("set base service %v(BaseProduct:%v,  BaseService:%v) error:%v", name, baseProduct, baseService, err_))
			continue
		}
		// it's for plugin
		if bsvc != nil {
			//sc.Service[name].Instance = bsvc.Instance
			svc := sc.Service[name]
			svc.Instance = bsvc.Instance
			sc.Service[name] = svc
		}
	}
	return err
}

func getK8SBaseServicInfo(s sqlxer, baseProduct, baseProductVersion, baseService, baseAttri string, clusterID int) (configMap schema.ConfigMap, ips, hosts []string, version string, err error) {
	var product []byte
	//don't care if product is parsed. get the product schema and then modify it.
	//so i can get the baseproduct's modify when it is changed and saved even if it is not deployed
	query := "SELECT p.product FROM " + model.TBL_DEPLOY_PRODUCT_LIST + " AS p LEFT JOIN " +
		model.TBL_DEPLOY_CLUSTER_PRODUCT_REL + " AS r ON p.id = r.pid " +
		"WHERE p.product_name = ? AND r.clusterId = ? AND r.is_deleted = 0"
	if err = s.Get(&product, query, baseProduct, clusterID); err != nil {
		if err != sql.ErrNoRows {
			log.Errorf("get product schema from deploy_product list err : %v", err)
		}
		dns := kmodel.BuildResourceName("service", "DTinsight", baseProduct, baseService)
		if baseAttri == BASE_SERVICE_OPTIONAL {
			configMap = nil
			ips = append(ips, dns)
			hosts = append(hosts, dns)
			err = nil
		}
		return
	}
	sc, err := schema.Unmarshal(product)
	if err != nil {
		return
	}
	var infoList []model.SchemaFieldModifyInfo
	query = "SELECT service_name, field_path, field FROM " + model.DeploySchemaFieldModify.TableName + " WHERE product_name=? AND cluster_id=?"
	if err = s.Select(&infoList, query, baseProduct, clusterID); err != nil {
		log.Errorf("query base service modify field err : %v", err)
		return
	}
	for _, info := range infoList {
		sc.SetField(info.ServiceName+"."+info.FieldPath, info.Field)
	}
	dns := kmodel.BuildResourceName("service", sc.ParentProductName, baseProduct, baseService)
	ips = append(ips, dns)
	hosts = append(hosts, dns)
	baseSvc := sc.Service[baseService]
	configMap = baseSvc.Config
	version = baseSvc.Version
	return
}

// 给非依赖服务组件设置ip和host为servicename以及修改变更后的一些字段配置信息
func setSchemaFieldDNS(clusterId int, sc *schema.SchemaConfig, s sqlxer, namespace string) error {
	var infoList []model.SchemaFieldModifyInfo
	var dns string
	query := "SELECT service_name, field_path, field FROM " + model.DeploySchemaFieldModify.TableName + " WHERE product_name=? AND cluster_id=? AND namespace=?"
	if err := model.USE_MYSQL_DB().Select(&infoList, query, sc.ProductName, clusterId, namespace); err != nil {
		return err
	}
	for _, modify := range infoList {
		sc.SetField(modify.ServiceName+"."+modify.FieldPath, modify.Field)
	}
	for name, svc := range sc.Service {
		if svc.BaseProduct != "" || svc.BaseService != "" {
			continue
		}
		// k8s模式下使用云服务主机资源
		var ipList string
		query = "SELECT ip_list FROM " + model.DeployServiceIpList.TableName + " WHERE product_name=? AND service_name=? AND cluster_id=? AND namespace=?"
		if err := s.Get(&ipList, query, sc.ProductName, name, clusterId, namespace); err != nil && err != sql.ErrNoRows {
			log.Errorf("[ServiceGroup->setSchemaFieldDNS] k8s use cloud mode database err:%v", err)
			return fmt.Errorf("k8s use cloud mode database err:%v", err)
		}

		if svc.Instance != nil && svc.Instance.UseCloud && ipList != "" {
			ips := strings.Split(ipList, IP_LIST_SEP)
			var hosts []string
			hosts = ips
			sc.SetServiceAddr(name, ips, hosts)
		} else {
			// 非使用云服务主机设置服务为service name
			switch sc.DeployType {
			case "workload":
				// 获取workload的类型信息
				wl, err := modelkube.WorkloadDefinition.Get(name, "")
				if err != nil {
					return fmt.Errorf("get workload type error: %v", err)
				}
				if wl == nil {
					// 若shcema产品包中的服务名不存在对应的workload type那么就用默认的命名规则
					buildMoleResourceServiceName("service", sc.ParentProductName, sc.ProductName, name, namespace, sc)
					continue
				}
				// 根据workload的id获取对应部署的k8s资源类型为deployment还是sts
				parts, err := modelkube.WorkloadPart.Select(wl.Id)
				if err != nil {
					return fmt.Errorf("get workload_part error: %v", err)
				}
				if parts == nil {
					return fmt.Errorf("get the part of workload type %s is null, please check the workload type", name)
				}
				//根据workload_part的ID获取对应service类型的steps
				workloadstep := []modelkube.WorloadStepSchema{}
				wkstep_query := "select * from workload_step where type='service' and workloadpart_id=?"
				if err := model.USE_MYSQL_DB().Select(&workloadstep, wkstep_query, parts[0].Id); err != nil {
					log.Errorf("get workload_step error:%v, sql:%v\n", err, wkstep_query)
					return fmt.Errorf("get workload_step error:%v, sql:%v\n", err, wkstep_query)
				}
				if workloadstep == nil {
					// 若shcema产品包中的服务名存在对应的workload type且对应的workload type没有定义对应的service资源，则使用默认的命名规则
					buildMoleResourceServiceName("service", sc.ParentProductName, sc.ProductName, name, namespace, sc)
					continue
				}

				switch parts[0].Type {
				case "statefulset":
					var stepSvcName string
					/*
						一个statefulset资源有可能存在两种service类型headless和cluster，若存在一种service类型即cluster类型的
						则直接获取该workload服务组件对应的service name，若存在两中类型的service则获取headless类型的service name
					*/
					if len(workloadstep) < 2 {
						dns = kmodel.BuildWorkloadServiceName(sc.ProductName, name, parts[0].Name, workloadstep[0].Name, namespace)
					} else {
						for _, stepSvc := range workloadstep {
							if exist := strings.Contains(stepSvc.Object, "clusterIP"); exist {
								stepSvcName = stepSvc.Name
							}
						}
						svcName := kmodel.BuildWorkloadServiceName(sc.ProductName, name, parts[0].Name, stepSvcName, namespace)
						//statefulset类型资源默认获取第一个pod为主角色
						podName := kmodel.BuildWorkloadPodName(sc.ProductName, name, parts[0].Name, "0")
						dns = podName + "." + svcName
					}
					if dns != "" {
						ips := strings.Split(dns, IP_LIST_SEP)
						sc.SetServiceAddr(name, ips, ips)
					}
				default:
					dns := kmodel.BuildWorkloadServiceName(sc.ProductName, name, parts[0].Name, workloadstep[0].Name, namespace)
					if dns != "" {
						ips := strings.Split(dns, IP_LIST_SEP)
						sc.SetServiceAddr(name, ips, ips)
					}
				}
			default:
				dns = kmodel.BuildResourceNameWithNamespace("service", sc.ParentProductName, sc.ProductName, name, namespace)
				if dns != "" {
					ips := strings.Split(dns, IP_LIST_SEP)
					sc.SetServiceAddr(name, ips, ips)
				}
			}

		}
	}
	return nil
}

func buildMoleResourceServiceName(resourceType, parentProductName, productName, serviceName, namespace string, sc *schema.SchemaConfig) {
	dns := kmodel.BuildResourceNameWithNamespace(resourceType, parentProductName, productName, serviceName, namespace)
	if dns != "" {
		ips := strings.Split(dns, IP_LIST_SEP)
		sc.SetServiceAddr(serviceName, ips, ips)
	}
}

// 处理当前集群指定namespace下产品包的参数修改信息
func setSchemafieldModifyInfo(clusterid int, sc *schema.SchemaConfig, namespace string) error {
	var infoList []model.SchemaFieldModifyInfo

	query := "SELECT service_name, field_path, field FROM " + model.DeploySchemaFieldModify.TableName + " WHERE cluster_id=? AND namespace=?"
	if err := model.USE_MYSQL_DB().Select(&infoList, query, clusterid, namespace); err != nil {
		return err
	}
	for _, modify := range infoList {
		if _, ok := sc.Service[modify.ServiceName]; ok {
			sc.SetField(modify.ServiceName+"."+modify.FieldPath, modify.Field)
		}
	}
	return nil
}

// 修改当前集群下指定namespace中的产品包依赖服务的ip地址信息
func BaseServiceAddrModify(clusterid int, sc *schema.SchemaConfig, namespace string) error {
	productName := sc.ProductName
	for _, svcName := range sc.GetBaseService() {
		err, svcipsTbsc := model.DeployServiceIpList.GetServiceIpListByName(productName, svcName, clusterid, namespace)
		if err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			log.Errorf("get serviceiplist wiht productname %s, servicename %s, clusterid %d fail, error: %v", productName, svcName, clusterid, err)
			return err
		}
		ips := strings.Split(svcipsTbsc.IpList, IP_LIST_SEP)
		sc.SetServiceAddr(svcName, ips, ips)
	}
	return nil
}

func DealK8SDeploy(namespace string, uncheckedServices []string, userId, clusterId int, relynamespace string, pid int) (rlt interface{}) {

	tx := model.USE_MYSQL_DB().MustBegin()
	defer func() {

		if _, ok := rlt.(error); ok {
			tx.Rollback()
		}
		if r := recover(); r != nil {
			tx.Rollback()
			rlt = r
		}
	}()
	cluster, err := model.DeployClusterList.GetClusterInfoById(clusterId)
	if err != nil {
		return fmt.Errorf("database err:%v", err)
	}

	// create log file for websocket read
	if !util.IsPathExist(kutil.BuildProductLogName(cluster.Name, namespace, clusterId, pid)) {
		os.Create(kutil.BuildProductLogName(cluster.Name, namespace, clusterId, pid))
	}

	product, err := model.DeployProductList.GetProductInfoById(pid)
	if err != nil {
		return fmt.Errorf("database err:%v", err)
	}

	var productListInfo model.DeployProductListInfo
	query := "SELECT id, product, parent_product_name FROM " + model.DeployProductList.TableName + " WHERE product_name=? AND product_version=?"
	if err := tx.Get(&productListInfo, query, product.ProductName, product.ProductVersion); err != nil {
		return err
	}

	sc, err := schema.Unmarshal(productListInfo.Product)
	if err != nil {
		return err
	}
	if err = inheritK8sBaseService(clusterId, relynamespace, sc, tx); err != nil {
		return err
	}
	if err = setSchemaFieldDNS(clusterId, sc, tx, namespace); err != nil {
		return err
	}
	//if the depened service is deployed in the same cluster.
	//and if the depened service ip is modified in one product.
	//all the other product depends on the base service will use the modified service ip.
	//but alse can modified again
	//if err = setBaseServiceAddr(baseClusterId, sc, tx); err != nil {
	//	return err
	//}
	if err = setSchemafieldModifyInfo(clusterId, sc, namespace); err != nil {
		return err
	}
	if err = BaseServiceAddrModify(clusterId, sc, namespace); err != nil {
		return err
	}
	if err = handleUncheckedServicesCore(sc, uncheckedServices); err != nil {
		return err
	}
	if err = sc.CheckServiceAddr(); err != nil {
		log.Errorf("%v", err)
		return err
	}
	//if need storage, but no storageclass is set. can't be deployed
	if err = sc.CheckStorage(); err != nil {
		return err
	}
	if sc.DeployType == "workload" && cluster.Mode == 0 {
		return fmt.Errorf("Self-built cluster is not compatible with Workload deployment")
	}
	err = model.DeployClusterProductRel.CheckProductReadyForDeploy(product.ProductName)
	if err != nil {
		return fmt.Errorf("database err:%v", err)
	}
	//store, err := model.ClusterImageStore.GetDefaultStoreByClusterId(clusterId)
	store, err := model.ClusterImageStore.GetStoreByClusterIdAndNamespace(clusterId, namespace)
	if err != nil {
		return fmt.Errorf("DealK8SDeploy GetStoreByClusterIdAndNamespace database err:%v", err)
	}

	currentProductRel, _ := model.DeployClusterProductRel.GetByPidAndClusterIdNamespacce(productListInfo.ID, clusterId, namespace)
	if currentProductRel.Status == model.PRODUCT_STATUS_DEPLOYING || currentProductRel.Status == model.PRODUCT_STATUS_UNDEPLOYING {
		return fmt.Errorf("product is deploying or undeploying, can't deploy again")
	}

	deployUUID := uuid.NewV4()
	rel := model.ClusterProductRel{
		Pid:        productListInfo.ID,
		ClusterId:  clusterId,
		Status:     model.PRODUCT_STATUS_DEPLOYING,
		DeployUUID: deployUUID.String(),
		UserId:     userId,
		Namespace:  namespace,
	}
	oldProductListInfo, err := model.DeployClusterProductRel.GetCurrentProductByProductNameClusterIdNamespace(product.ProductName, clusterId, namespace)
	if err == nil {
		query = "UPDATE " + model.DeployClusterProductRel.TableName + " SET pid=?, user_id=?, `status`=?, `deploy_uuid`=?, namespace=?, deploy_time=NOW() WHERE pid=? AND clusterId=? AND is_deleted=0 AND namespace=?"
		if _, err := tx.Exec(query, productListInfo.ID, userId, model.PRODUCT_STATUS_DEPLOYING, deployUUID, namespace, oldProductListInfo.ID, clusterId, namespace); err != nil {
			log.Errorf("%v", err)
			return err
		}
	} else if err == sql.ErrNoRows {
		query = "INSERT INTO " + model.DeployClusterProductRel.TableName + " (pid, clusterId, deploy_uuid, user_id, deploy_time, status, namespace) VALUES" +
			" (:pid, :clusterId, :deploy_uuid, :user_id, NOW(), :status, :namespace)"
		if _, err = tx.NamedExec(query, &rel); err != nil {
			log.Errorf("%v", err)
			return err
		}
	} else {
		log.Errorf("%v", err)
		return err
	}
	// 将产品包中未选择的服务的列表写入deploy_unchecked_service
	if len(uncheckedServices) > 0 {
		uncheckedServiceInfo := model.DeployUncheckedServiceInfo{ClusterId: clusterId, Pid: productListInfo.ID, UncheckedServices: strings.Join(uncheckedServices, ","), Namespace: namespace}
		query = "INSERT INTO " + model.DeployUncheckedService.TableName + " (pid, cluster_id, unchecked_services, namespace) VALUES" +
			" (:pid, :cluster_id, :unchecked_services, :namespace) ON DUPLICATE KEY UPDATE unchecked_services=:unchecked_services,namespace=:namespace, update_time=NOW()"
		if _, err = tx.NamedExec(query, &uncheckedServiceInfo); err != nil {
			log.Errorf("%v", err)
			return err
		}
	} else {
		query = "DELETE FROM " + model.DeployUncheckedService.TableName + " WHERE pid=? AND cluster_id=? AND namespace=?"
		if _, err = tx.Exec(query, productListInfo.ID, clusterId, namespace); err != nil && err != sql.ErrNoRows {
			log.Errorf("%v", err)
			return err
		}
	}
	productHistoryInfo := model.DeployProductHistoryInfo{
		ClusterId:          clusterId,
		DeployUUID:         deployUUID,
		ProductName:        product.ProductName,
		ProductNameDisplay: productListInfo.ProductNameDisplay,
		ProductVersion:     product.ProductVersion,
		Status:             model.PRODUCT_STATUS_DEPLOYING,
		ParentProductName:  productListInfo.ParentProductName,
		UserId:             userId,
		Namespace:          namespace,
		ProductType:        1,
	}
	sc.ParentProductName = productListInfo.ParentProductName

	query = "INSERT INTO " + model.DeployProductHistory.TableName + " (cluster_id, product_name, product_name_display, deploy_uuid, product_version, `status`, parent_product_name, deploy_start_time, user_id, namespace, product_type) " +
		"VALUES (:cluster_id, :product_name, :product_name_display, :deploy_uuid, :product_version, :status , :parent_product_name, NOW(), :user_id, :namespace, :product_type)"
	if _, err := tx.NamedExec(query, &productHistoryInfo); err != nil {
		log.Errorf("%v", err)
		return err
	}
	if err := tx.Commit(); err != nil {
		log.Errorf("%v", err)
		return err
	}

	//所有的list 接口用到接收的 uuid 参数 都要在该表中有记录 用以判断该 uuid 类型
	err = model.DeployUUID.InsertOne(deployUUID.String(), "", model.ManualDeployUUIDType, pid)
	if err != nil {
		log.Errorf("%v", err)
		return nil
	}

	// TODO push docker image
	go k8sDeploy(sc, deployUUID, pid, uncheckedServices, clusterId, namespace, store, relynamespace, cluster.Name)

	return map[string]interface{}{"deploy_uuid": deployUUID}
}

func k8sDeploy(sc *schema.SchemaConfig, deployUUID uuid.UUID, pid int, uncheckedServices []string, clusterId int, namespace string, store model.ImageStore, relynamespace, clusterName string) error {
	var err error
	var query string

	fileName := kutil.BuildProductLogName(clusterName, namespace, clusterId, pid)
	logf, err := os.OpenFile(fileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0766)
	defer logf.Close()
	if err == nil {
		log.NewOutputPath(deployUUID.String(), logf)
		// defer log.CloseOutputPath(deployUUID.String())
	} else {
		log.Errorf(err.Error())
	}

	log.OutputInfof(deployUUID.String(), "%v", LINE_LOG)

	// lock product with clusterId namespace
	lock := model.KubeProductLock{
		ClusterId: clusterId,
		Pid:       pid,
		Namespace: namespace,
		IsDeploy:  1,
	}
	err = model.DeployKubeProductLock.InsertOrUpdateRecord(lock)
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	//temp code
	var baseproduct int
	useCloudService := map[string]schema.ServiceConfig{}
	for name, svc := range sc.Service {
		if svc.BaseProduct != "" && svc.BaseService != "" {
			baseproduct = baseproduct + 1
			continue
		}
		if svc.Instance.UseCloud {
			useCloudService[name] = svc
			delete(sc.Service, name)
		}
	}

	defer func() {
		// unlock product with clusterId namespace
		lock := model.KubeProductLock{
			ClusterId: clusterId,
			Pid:       pid,
			Namespace: namespace,
			IsDeploy:  0,
		}
		// wait deploy
		//time.Sleep(5 * time.Second)
		model.DeployKubeProductLock.InsertOrUpdateRecord(lock)

		//tmp code
		for name, svc := range useCloudService {
			sc.Service[name] = svc
		}

		if err != nil {
			status := model.PRODUCT_STATUS_DEPLOY_FAIL
			query = "UPDATE " + model.DeployProductHistory.TableName + " SET `status`=?, deploy_end_time=NOW() WHERE deploy_uuid=? AND cluster_id=?"
			if _, err1 := model.DeployProductHistory.GetDB().Exec(query, status, deployUUID, clusterId); err1 != nil {
				log.Errorf("%v", err1.Error())
			}
			query = "UPDATE " + model.DeployClusterProductRel.TableName + " SET status=?, update_time=NOW() WHERE pid=? AND clusterId=? AND namespace=? AND is_deleted=0"
			if _, err := model.DeployClusterProductRel.GetDB().Exec(query, status, pid, clusterId, namespace); err != nil {
				log.Errorf("%v", err)
			}
		}

		if err_p := sc.ParseVariable(); err_p == nil {
			productParsed, err_j := json.Marshal(sc)
			if err_j != nil {
				log.Errorf("%v, %v", err_j.Error(), productParsed)
			}
			query = "UPDATE " + model.DeployClusterProductRel.TableName + " SET product_parsed=?, update_time=NOW() WHERE pid=? AND clusterId=? AND namespace=? AND is_deleted=0"
			if _, errq := model.DeployClusterProductRel.GetDB().Exec(query, productParsed, pid, clusterId, namespace); errq != nil {
				log.Errorf("%v", errq.Error())
			}
		} else {
			log.Errorf("%v", err_p.Error())
		}
	}()

	log.Infof("cluster %v installing new instance and rolling update ...", clusterId)
	log.OutputInfof(deployUUID.String(), "cluster %v installing new instance and rolling update ...", clusterName)
	//imageStore, err := model.ClusterImageStore.GetDefaultStoreByClusterId(clusterId)
	//if err != nil {
	//	log.Errorf("%v", err)
	//	return err
	//}
	//push images
	err = kdeploy.PushImages(store, sc, deployUUID.String())
	if err != nil {
		log.Errorf("%v", err)
	}
	//import cluster to interact witht apiserver
	var cache kube.ClientCache
	namspaceInfo, err := modelkube.DeployNamespaceList.Get(namespace, clusterId)
	if err != nil {
		return err
	}
	if namspaceInfo != nil {
		cache, err = kube.ClusterNsClientCache.GetClusterNsClient(strconv.Itoa(clusterId)).GetClientCache(kube.ImportType(namspaceInfo.Type))
		if err != nil {
			return err
		}
	}

	//temp code
	if len(useCloudService) != 0 {
		for name, cloudsvc := range useCloudService {

			ipAndPortList := cloudsvc.ServiceAddr.IP
			servicePortList := make([]corev1.ServicePort, 0, len(ipAndPortList))
			endpointsAddressList := []corev1.EndpointAddress{}
			endpointsPortList := []corev1.EndpointPort{}
			for i, str := range ipAndPortList {
				ipAndPort := strings.Split(str, ":")
				ip := ipAndPort[0]
				var port int32
				if len(ipAndPort) == 1 {
					port = 80
				} else {
					ipInt, _ := strconv.Atoi(ipAndPort[1])
					port = int32(ipInt)
				}

				servicePort := corev1.ServicePort{
					Name:       "port" + strconv.Itoa(i),
					Protocol:   "TCP",
					Port:       port,
					TargetPort: intstr.FromInt(int(port)),
				}

				servicePortList = append(servicePortList, servicePort)

				endpointsAddressList = append(endpointsAddressList, corev1.EndpointAddress{IP: ip})
				endpointsPortList = append(endpointsPortList, corev1.EndpointPort{Port: port})

			}

			svc := service.New()
			svc.Name = fmt.Sprintf("%s-%s-%s", "cloudsvc", strings.ToLower(sc.ProductName), strings.ToLower(name))
			svc.Namespace = namespace
			svc.Spec.Ports = servicePortList

			endpoints := endpoints.New()
			endpoints.Name = svc.Name
			endpoints.Namespace = namespace
			endpoints.Subsets = []corev1.EndpointSubset{
				{
					Addresses: endpointsAddressList,
					Ports:     endpointsPortList,
				},
			}

			cache.GetClient(namespace).Apply(sysContext.TODO(), svc)
			cache.GetClient(namespace).Apply(sysContext.TODO(), endpoints)

			instancelist := model.DeployInstanceInfo{
				ClusterId:   clusterId,
				Namespace:   namespace,
				Pid:         pid,
				Ip:          cloudsvc.ServiceAddr.IP[0],
				ServiceName: name,
				Status:      "running",
				HealthState: 1,
			}
			inst := model.DeployInstanceInfo{}
			getquery := "select * from " + model.DeployInstanceList.TableName + " where cluster_id= ? and namespace= ? and pid= ? and service_name= ?"
			if err := model.USE_MYSQL_DB().Get(&inst, getquery, clusterId, namespace, pid, name); err == sql.ErrNoRows {
				insertquery := "INSERT INTO " + model.DeployInstanceList.TableName + " (cluster_id,namespace,pid,ip,service_name,status,health_state) VALUES" +
					" (:cluster_id, :namespace , :pid, :ip ,:service_name, :status, :health_state)"
				if _, err = model.USE_MYSQL_DB().NamedExec(insertquery, &instancelist); err != nil {
					log.Errorf("%v", err)
					return err
				}
			} else {
				updatequery := "update " + model.DeployInstanceList.TableName + " set ip= :ip where cluster_id= :cluster_id and namespace= :namespace and pid= :pid and service_name= :service_name"
				if _, err = model.USE_MYSQL_DB().NamedExec(updatequery, &instancelist); err != nil {
					log.Errorf("%v", err)
					return err
				}
			}

			//err, _, _ := model.DeployInstanceList.NewPodInstanceRecord(clusterId,pid,0,1,namespace,cloudsvc.ServiceAddr.IP[0],"","",name,"","","running","",nil)
			//if err != nil {
			//	return fmt.Errorf("k8s use cloud database NewPodInstanceRecord error: %v",err)
			//}
		}
	}

	if len(sc.Service) == 0 || len(sc.Service) == baseproduct {
		err = model.DeployClusterProductRel.UpdateStatusWithNamespace(clusterId, pid, namespace, model.PRODUCT_STATUS_DEPLOYED)
		if err != nil {
			log.Errorf("nothing changed, update the product deploy status fail, error %v", err)
			return err
		}
		query := "UPDATE " + model.DeployProductHistory.TableName + " SET `status`=?, deploy_end_time=NOW() WHERE deploy_uuid=? AND cluster_id=? AND namespace=?"
		if _, err := model.DeployProductHistory.GetDB().Exec(query, model.PRODUCT_STATUS_DEPLOYED, deployUUID.String(), clusterId, namespace); err != nil {
			log.Errorf("nothing changed, update the history status fail, error %v", err)
		}
		return nil
	}
	//tmpcode
	////////////////////////////

	// deploy secret
	//log.OutputInfof(deployUUID.String(), "starting apply secret...")
	//err = kdeploy.ApplyImageSecret(cache, clusterId, namespace, store)
	//if err != nil {
	//	log.Errorf("%v", err)
	//	log.OutputInfof(deployUUID.String(), "apply secret error:%v", err)
	//	return err
	//}
	//log.OutputInfof(deployUUID.String(), "apply secret success")

	if sc.DeployType == "workload" {

		ifchanged := false
		ifchanged, err = kdeploy.ApplyWorkloadProcess(cache, sc, uncheckedServices, namespace, deployUUID.String(), pid, clusterId, &store)
		if err != nil {
			log.OutputInfof(deployUUID.String(), "apply workload error %v", err)
			return err
		}
		log.OutputInfof(deployUUID.String(), "apply workload success")
		// if the workload exist on k8s, compare current with last, if all workload is not changed, success deployed directly
		if !ifchanged {
			err = model.DeployClusterProductRel.UpdateStatusWithNamespace(clusterId, pid, namespace, model.PRODUCT_STATUS_DEPLOYED)
			if err != nil {
				log.Errorf("nothing changed, update the product deploy status fail, error %v", err)
				return err
			}
			query := "UPDATE " + model.DeployProductHistory.TableName + " SET `status`=?, deploy_end_time=NOW() WHERE deploy_uuid=? AND cluster_id=? AND namespace=?"
			if _, err := model.DeployProductHistory.GetDB().Exec(query, model.PRODUCT_STATUS_DEPLOYED, deployUUID.String(), clusterId, namespace); err != nil {
				log.Errorf("nothing changed, update the history status fail, error %v", err)
			}
		}
		return nil
	}

	// deploy configMap
	log.OutputInfof(deployUUID.String(), "starting apply configMap...")
	err = kdeploy.ApplyConfigMaps(cache, sc, clusterId, namespace)
	if err != nil {
		log.Errorf("%v", err)
		log.OutputInfof(deployUUID.String(), "apply configMap error:%v", err)
		return err
	}
	log.OutputInfof(deployUUID.String(), "apply configMap success")

	// deploy mole
	log.OutputInfof(deployUUID.String(), "starting apply mole...")
	err = kdeploy.ApplyMole(cache, sc, uncheckedServices, clusterId, pid, namespace, deployUUID.String(), store.Alias)
	if err != nil {
		log.Errorf("%v", err)
		log.OutputInfof(deployUUID.String(), "apply mole error:%v", err)
		return err
	}
	log.OutputInfof(deployUUID.String(), "apply mole success")
	if relynamespace != "" {
		kubeBase := model.KubeBaseProduct{
			Pid:           pid,
			ClusterId:     clusterId,
			Namespace:     namespace,
			RelyNamespace: relynamespace,
		}
		err = model.DeployKubeBaseProduct.InsertRecord(kubeBase)
		if err != nil {
			log.Errorf("%v", err.Error())
			return err
		}
	}
	return nil
}

// Undeploy success situation:
// 1 mole not exist
// 2 instance list is null
// 3 instance list delete success
func K8sUndeploy(clusterId int, sc *schema.SchemaConfig, deployUUID uuid.UUID, pid int, namespace string) {
	var err error
	var query string
	var cache kube.ClientCache
	namspaceInfo, err := modelkube.DeployNamespaceList.Get(namespace, clusterId)
	if err != nil {
		log.Errorf("[cluster]: get namespaceinfo with ns %s, clusterid %d ,occur error", namespace, clusterId)
	}
	if namspaceInfo != nil {
		cache, err = kube.ClusterNsClientCache.GetClusterNsClient(strconv.Itoa(clusterId)).GetClientCache(kube.ImportType(namspaceInfo.Type))
		if err != nil {
			log.Errorf("[cluster]: get client cache in clusterid %d, type %s", clusterId, namspaceInfo.Type)
		}
	}
	defer func() {
		if err != nil {
			status := model.PRODUCT_STATUS_UNDEPLOY_FAIL
			query = "UPDATE " + model.DeployClusterProductRel.TableName + " SET status=?, update_time=NOW() WHERE pid=? AND clusterId=? AND namespace=? AND is_deleted=0"
			if _, err := model.DeployClusterProductRel.GetDB().Exec(query, status, pid, clusterId, namespace); err != nil {
				log.Errorf("%v", err)
			}
			query = "UPDATE " + model.DeployProductHistory.TableName + " SET `status`=?, deploy_end_time=NOW() WHERE deploy_uuid=? AND cluster_id=? AND namespace=?"
			if _, err := model.DeployProductHistory.GetDB().Exec(query, status, deployUUID, clusterId, namespace); err != nil {
				log.Errorf("%v", err)
			}
		}
	}()

	log.Infof("cluster %v undeploy instance ...", clusterId)
	var obj interface{}
	if sc.DeployType == "workload" {
		workloadProcess, _ := kdeploy.GetWorkloadProcess(cache, sc, namespace)
		if workloadProcess != nil {
			obj = workloadProcess
		}
	} else {
		mole, _ := kdeploy.GetMole(cache, sc, clusterId, namespace)
		if mole != nil {
			obj = mole
		}
	}
	if obj == nil {
		//1， Mole 不存在，强行卸载成功
		log.Infof("target mole not exist, clear deploy history...")
		query := "UPDATE " + model.DeployProductHistory.TableName + " SET `status`=?, deploy_end_time=NOW() WHERE deploy_uuid=? and status=?"
		if _, err := model.DeployProductHistory.GetDB().Exec(query, model.PRODUCT_STATUS_UNDEPLOYED, deployUUID, model.PRODUCT_STATUS_UNDEPLOYING); err != nil {
			log.Errorf("%v", err)
		}
		query = "UPDATE " + model.DeployClusterProductRel.TableName + " SET status=?, is_deleted=?, update_time=NOW() WHERE pid=? AND clusterId=? AND namespace=?"
		if _, err := model.DeployClusterProductRel.GetDB().Exec(query, model.PRODUCT_STATUS_UNDEPLOYED, 1, pid, clusterId, namespace); err != nil {
			log.Errorf("%v", err)
		}
		// delete instance list
		err = model.DeployInstanceList.DeleteByClusterIdPidNamespace(pid, clusterId, namespace)
		if err != nil {
			log.Errorf("clear old instance err %v", err.Error())
		}
		// delete service ip list
		err = model.DeployServiceIpList.DeleteByClusterIdNamespaceProduct(namespace, sc.ProductName, clusterId)
		if err != nil {
			log.Errorf("clear old service ip list err %v", err.Error())
		}
		return
	}
	instanceList, _ := model.DeployInstanceList.GetInstanceListByClusterIdNamespace(clusterId, pid, namespace)
	for _, instance := range instanceList {
		instanceRecordInfo := &model.DeployInstanceRecordInfo{
			DeployUUID:         deployUUID,
			InstanceId:         instance.ID,
			Sid:                instance.Sid,
			Ip:                 instance.Ip,
			ProductName:        sc.ProductName,
			ProductVersion:     sc.ProductVersion,
			ProductNameDisplay: sc.ProductNameDisplay,
			Group:              instance.Group,
			ServiceName:        instance.ServiceName,
			ServiceVersion:     instance.ServiceVersion,
			ServiceNameDisplay: instance.ServiceName,
			Status:             model.INSTANCE_STATUS_UNINSTALLING,
			Progress:           0,
		}
		err, _, _ = model.DeployInstanceRecord.CreateOrUpdate(instanceRecordInfo)

		if err != nil {
			log.Errorf("update instance record uninstalling fail,err: %v", err)
			return
		}
	}

	if sc.DeployType == "workload" {
		kdeploy.DeleteWorkloadProcess(cache, sc.ProductName, namespace)
	} else {
		if err = kdeploy.DeleteMole(cache, sc.ProductName, namespace, clusterId); err != nil {
			log.Errorf("%v undeploy error: %v", deployUUID, err)
			return
		}
	}

	if len(instanceList) == 0 {
		//没有历史服务残留，强行卸载成功
		query := "UPDATE " + model.DeployProductHistory.TableName + " SET `status`=?, deploy_end_time=NOW() WHERE deploy_uuid=? and status=?"
		if _, err := model.DeployProductHistory.GetDB().Exec(query, model.PRODUCT_STATUS_UNDEPLOYED, deployUUID, model.PRODUCT_STATUS_UNDEPLOYING); err != nil {
			log.Errorf("%v", err)
		}
		query = "UPDATE " + model.DeployClusterProductRel.TableName + " SET status=?, is_deleted=?, update_time=NOW() WHERE pid=? AND clusterId=? AND namespace=?"
		if _, err := model.DeployClusterProductRel.GetDB().Exec(query, model.PRODUCT_STATUS_UNDEPLOYED, 1, pid, clusterId, namespace); err != nil {
			log.Errorf("%v", err)
		}
	}
	log.Infof("undeploy %v(%v) success", sc.ProductName, sc.ProductVersion)
}

func StopUndeployingK8sProduct(ctx context.Context) apibase.Result {
	log.Debugf("StopUndeployingK8sProduct: %v", ctx.Request().RequestURI)

	clusterId, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		return fmt.Errorf("cluster_id is empty")
	}
	pid, err := ctx.Params().GetInt("pid")
	if err != nil {
		return fmt.Errorf("pid is empty")
	}
	namespace := ctx.Params().Get("namespace_name")
	//if namespace == "" {
	//	return fmt.Errorf("namespace is empty")
	//}

	productRel, err := model.DeployClusterProductRel.GetByPidAndClusterIdNamespacce(pid, clusterId, namespace)
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	status := model.PRODUCT_STATUS_UNDEPLOY_FAIL
	query := "UPDATE " + model.DeployClusterProductRel.TableName + " SET status=?, update_time=NOW() WHERE pid=? AND clusterId=? AND namespace=? AND is_deleted=0"
	if _, err := model.DeployClusterProductRel.GetDB().Exec(query, status, pid, clusterId, namespace); err != nil {
		log.Errorf("%v", err)
		return err
	}
	query = "UPDATE " + model.DeployProductHistory.TableName + " SET `status`=?, deploy_end_time=NOW() WHERE deploy_uuid=? AND cluster_id=? AND namespace=?"
	if _, err := model.DeployProductHistory.GetDB().Exec(query, status, productRel.DeployUUID, clusterId, namespace); err != nil {
		log.Errorf("%v", err)
		return err
	}
	return nil
}

func GetK8sClusterInstallLog(ctx context.Context) apibase.Result {
	log.Debugf("GetK8sClusterInstallLog: %v", ctx.Request().RequestURI)

	clusterId, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		log.Errorf("%v", err)
		return err
	}
	cluster, err := model.DeployClusterList.GetClusterInfoById(clusterId)
	if err != nil {
		return fmt.Errorf("clusterId is empty")
	}

	ws, err := ksocket.NewSocket(ctx)
	if err != nil {
		return err
	}

	go ksocket.SocketWriter(ws, time.Unix(0, 0), kutil.BuildClusterLogName(cluster.Name, cluster.Id))
	ksocket.SocketReader(ws)
	return nil
}

func GetProductInstallLog(ctx context.Context) apibase.Result {
	log.Debugf("GetProductInstallLog: %v", ctx.Request().RequestURI)

	clusterId, err := ctx.Params().GetInt("cluster_id")
	if err != nil {
		return fmt.Errorf("cluster_id is empty")
	}
	pid, err := ctx.Params().GetInt("pid")
	if err != nil {
		return fmt.Errorf("pid is empty")
	}
	namespace := ctx.Params().Get("namespace_name")
	if namespace == "" {
		return fmt.Errorf("namespace is empty")
	}
	cluster, err := model.DeployClusterList.GetClusterInfoById(clusterId)
	if err != nil {
		return fmt.Errorf("clusterId is empty")
	}

	ws, err := ksocket.NewSocket(ctx)
	if err != nil {
		return err
	}

	go ksocket.SocketWriter(ws, time.Unix(0, 0), kutil.BuildProductLogName(cluster.Name, namespace, cluster.Id, pid))
	ksocket.SocketReader(ws)

	return nil
}

func GetClusterProductsInfo(ctx context.Context) apibase.Result {
	log.Debugf("GetProductGroupList: %v", ctx.Request().RequestURI)
	clusterId, err := ctx.URLParamInt("clusterId")
	if err != nil {
		return fmt.Errorf("get clusterId err %v", err.Error())
	}
	parentProductName := ctx.URLParam("parentProductName")
	if parentProductName == "" {
		return fmt.Errorf("parentProductName null")
	}
	products, err := model.DeployClusterProductRel.GetProductsByParentProductNameClusterId(parentProductName, clusterId)
	if err != nil {
		log.Errorf("%v", err.Error())
		return err
	}
	list := []map[string]interface{}{}
	for _, s := range products {
		m := map[string]interface{}{}
		sc, err := schema.Unmarshal(s.ProductParsed)
		if err != nil {
			log.Errorf("[GetClusterProductsInfo] Unmarshal err: %v", err)
			continue
		}
		m["id"] = s.ID
		m["product_name"] = s.ProductName
		m["product_name_display"] = s.ProductNameDisplay
		m["product_version"] = s.ProductVersion
		m["services"] = sc

		if s.UserId > 0 {
			if err, userInfo := model.UserList.GetInfoByUserId(s.UserId); err != nil {
				m["username"] = ""
			} else {
				m["username"] = userInfo.UserName
			}
		} else {
			m["username"] = ""
		}
		m["status"] = s.Status
		m["deploy_uuid"] = s.DeployUUID
		m["product_type"] = s.ProductType
		if s.DeployTime.Valid == true {
			m["deploy_time"] = s.DeployTime.Time.Format(base.TsLayout)
		} else {
			m["deploy_time"] = ""
		}
		if s.CreateTime.Valid == true {
			m["create_time"] = s.CreateTime.Time.Format(base.TsLayout)
		} else {
			m["create_time"] = ""
		}
		list = append(list, m)
	}
	productsJson, err := json.Marshal(list)
	if err != nil {
		log.Errorf("%v", err.Error())
		return err
	}
	cluster, err := model.DeployClusterList.GetClusterInfoById(clusterId)
	if err != nil {
		log.Errorf("%v", err.Error())
		return err
	}
	saveTo := cluster.Name + "_" + parentProductName + "_" + time.Now().Format("20060102150405") + ".json"
	ctx.Header("Content-Disposition", "attachment;filename="+saveTo)
	ctx.Write(productsJson)
	return apibase.EmptyResult{}
}

func GetRestartServices(ctx context.Context) apibase.Result {
	clusterId, err := GetCurrentClusterId(ctx)
	if err != nil {
		log.Errorf("%v", err)
		return nil
	}

	type restartServiceInfo struct {
		ProductName       string `json:"product_name" db:"product_name"`
		ServiceName       string `json:"service_name" db:"service_name"`
		DependProductName string `json:"depend_product_name" db:"depend_product_name"`
		DependServiceName string `json:"depend_service_name" db:"depend_service_name"`
	}

	restartServiceList := make([]restartServiceInfo, 0)
	query := "select distinct(service_name), product_name, depend_product_name, depend_service_name from " + model.TBL_NOTIFY_EVENT +
		" where cluster_id=? and is_deleted=0"
	if err := model.USE_MYSQL_DB().Select(&restartServiceList, query, clusterId); err != nil {
		log.Errorf("%v", err)
		return err
	}
	return map[string]interface{}{
		"list":  restartServiceList,
		"count": len(restartServiceList),
	}
}

func GetCurrentExecCount(ctx context.Context) apibase.Result {
	clusterId, err := ctx.URLParamInt("clusterId")
	if err != nil {
		log.Errorf("clusterId is empty")
		return nil
	}
	count, err := model.OperationList.GetRunningCount(clusterId)
	if err != nil {
		return fmt.Errorf("获取当前运行总数异常：%v", err)
	}
	return count
}

func OrderList(ctx context.Context) apibase.Result {
	var whereCause []string
	var baseSqlStr, countSqlStr, whereCauseSqlStr, pageSqlStr, orderSqlStr string

	urlParams := ctx.URLParams()
	if clusterId, ok := urlParams["clusterId"]; ok {
		whereCause = append(whereCause, " cluster_id = "+clusterId)
	} else {
		return fmt.Errorf("clusterId is empty")
	}

	if startTime, ok := urlParams["startTime"]; ok {
		whereCause = append(whereCause, fmt.Sprintf(" create_time > '%s' ", startTime))
	}

	if endTime, ok := urlParams["endTime"]; ok {
		whereCause = append(whereCause, fmt.Sprintf(" end_time < '%s' ", endTime))
	}

	if objectValue, ok := urlParams["objectValue"]; ok {

		//如果是 ip 这转化为 sid
		//address := net.ParseIP(objectValue)
		//if address != nil {
		//	err, info := model.DeployHostList.GetHostInfoByIp(objectValue)
		//	if err != nil {
		//		return fmt.Errorf("DeployHostList GetHostInfoByIp error: %v", err)
		//	}
		//	objectValue = info.SidecarId
		//}
		whereCause = append(whereCause, fmt.Sprintf(" object_value = '%s' ", objectValue))
	}
	if operationType, ok := urlParams["operationType"]; ok {
		whereCause = append(whereCause, " operation_type = "+operationType)
	}

	if status, ok := urlParams["status"]; ok {
		whereCause = append(whereCause, " operation_status = "+status)
	}

	baseSqlStr = fmt.Sprintf("select * from %s  where ", model.OPERATION_LIST)
	whereCauseSqlStr = strings.Join(whereCause, " and ")

	page, pageOk := urlParams["page"]
	pageSize, pageSizeOk := urlParams["pageSize"]
	if pageOk && pageSizeOk {
		pageInt, err := strconv.Atoi(page)
		if err != nil {
			return fmt.Errorf("page is not a number")
		}
		pageSizeInt, err := strconv.Atoi(pageSize)
		if err != nil {
			return fmt.Errorf("pageSize is not a number")
		}
		pageSqlStr = fmt.Sprintf(" limit %d offset %d ", pageSizeInt, (pageInt-1)*pageSizeInt)
	} else {
		return fmt.Errorf("page or pageSize is empty")
	}
	orderSqlStr = " order by create_time desc "
	fullSql := baseSqlStr + whereCauseSqlStr + orderSqlStr + pageSqlStr

	var count int
	countSqlStr = fmt.Sprintf("select count(1) from %s  where ", model.OPERATION_LIST)
	err := model.OperationList.GetDB().Get(&count, countSqlStr+whereCauseSqlStr)
	if err != nil {
		return fmt.Errorf("OrderList get count: %v", err)
	}

	operationList := []model.OperationInfo{}

	err = model.OperationList.GetDB().Select(&operationList, fullSql)
	if err != nil {
		return fmt.Errorf("OperationList query err: %v", err)
	}

	clusterIdStr := urlParams["clusterId"]
	clusterIdInt, err := strconv.Atoi(clusterIdStr)
	if err != nil {
		return fmt.Errorf("strconv.Atoi clusterIdStr error %v", err)
	}

	for idx, operationInfo := range operationList {
		enum, err := enums.OperationType.GetByCode(operationInfo.OperationType)
		if err != nil {
			return err
		}
		operationList[idx].OperationName = enum.Desc
		operationList[idx].ShowCreateTime = operationList[idx].CreateTime.Time.Format(TIME_LAYOUT)

		if operationList[idx].EndTime.Valid {
			operationList[idx].ShowEndTime = operationList[idx].EndTime.Time.Format(TIME_LAYOUT)
		}

		if operationList[idx].Duration.Valid {
			operationList[idx].ShowDuration = operationList[idx].Duration.Float64
		} else {
			operationList[idx].ShowDuration = time.Now().Sub(operationList[idx].CreateTime.Time).Seconds()
		}
		operationList[idx].ShowDuration = util.Float64OneDecimalPlaces(operationList[idx].ShowDuration)

		//如果 object value 是 sid  则将 sid 转为 ip
		//_, err = uuid.FromString(operationList[idx].ObjectValue)
		//if err == nil {
		//	err, info := model.DeployHostList.GetHostInfoBySid(operationList[idx].ObjectValue)
		//	if err != nil {
		//		return fmt.Errorf("DeployHostList GetHostInfoBySid error: %v", err)
		//	}
		//	operationList[idx].ObjectValue = info.Ip
		//}
		execInfo, err := model.ExecShellList.GetByOperationId(operationList[idx].OperationId)

		if errors.Is(err, sql.ErrNoRows) {
			log.Errorf("未查询到 operationId=%s的shell", operationList[idx].OperationId)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("ExecShellList GetByOperationId error: %v", err)
		}

		if execInfo != nil {
			services, err := getGroupAndServices(clusterIdInt, execInfo.ProductName)
			if err != nil {
				log.Errorf("getGroupAndServices error: %v", err)
			}
			if operationList[idx].ObjectType == enums.OperationObjType.Svc.Code {
				operationList[idx].ProductName = execInfo.ProductName
				operationList[idx].Group, operationList[idx].ParentProductName = getGroupBySvcName(services, operationList[idx].ObjectValue)
				pInfo, _ := model.DeployClusterProductRel.GetCurrentProductByProductNameClusterIdNamespace(execInfo.ProductName, clusterIdInt, "")
				if pInfo.ProductName != "" {
					operationList[idx].IsExist = true
				}
			}
		}

		if operationList[idx].ObjectType == enums.OperationObjType.Product.Code {
			services, err := getGroupAndServices(clusterIdInt, operationList[idx].ObjectValue)
			if err != nil {
				log.Errorf("getGroupAndServices error: %v", err)
			}
			_, operationList[idx].ParentProductName = getGroupBySvcName(services, "")
			operationList[idx].ProductName = operationList[idx].ObjectValue
			pInfo, _ := model.DeployClusterProductRel.GetCurrentProductByProductNameClusterIdNamespace(operationList[idx].ObjectValue, clusterIdInt, "")
			if pInfo.ProductName != "" {
				operationList[idx].IsExist = true
			}
		}

		if operationList[idx].ObjectType == enums.OperationObjType.Host.Code {
			err, _ := model.DeployHostList.GetHostInfoByIp(operationList[idx].ObjectValue)
			if err == nil {
				operationList[idx].IsExist = true
			}
		}

	}
	return map[string]interface{}{
		"count": count,
		"list":  operationList,
	}
}

type ChildrenResp struct {
	Name              string         `json:"name"`
	ProductName       string         `json:"productName"`
	OperationType     int            `json:"operationType"`
	ShellType         int            `json:"shellType"`
	ObjectValue       string         `json:"objectValue"`
	ExecId            string         `json:"execId"`
	HostIp            string         `json:"hostIp"`
	ObjectType        int            `json:"objectType"`
	Status            int            `json:"status"`
	Group             string         `json:"group"`
	ParentProductName string         `json:"parentProductName"`
	IsExist           bool           `json:"isExist"`
	Desc              string         `json:"desc"`
	StartTime         string         `json:"startTime"`
	EndTime           string         `json:"endTime"`
	Duration          float64        `json:"duration"`
	ChildrenResp      []ChildrenResp `json:"children"`
}
type resultList struct {
	ServiceName        string `json:"service_name"`
	ServiceNameDisplay string `json:"service_name_display"`
	ParentProductName  string `json:"parent_product_name"`
	Alert              bool   `json:"alert"`
}

func getGroupAndServices(clusterId int, productName string) (map[string][]resultList, error) {

	type serviceInfo struct {
		ServiceName string `db:"service_name"`
		Group       string `db:"group"`
		HealthState int    `db:"health_state"`
		Status      string `db:"status"`
	}

	groupAndServices := map[string][]resultList{}
	serviceInfoList := []serviceInfo{}

	// Avoid deploying the same product package with multiple namespaces
	query := "SELECT IL.service_name, IL.group, IL.health_state, IL.status FROM " +
		model.DeployInstanceList.TableName + " AS IL LEFT JOIN " + model.DeployProductList.TableName + " AS PL ON IL.pid = PL.id WHERE PL.product_name=? AND IL.cluster_id=? AND IL.namespace=? ORDER BY service_name"
	if err := model.USE_MYSQL_DB().Select(&serviceInfoList, query, productName, clusterId, ""); err != nil {
		log.Errorf("%v", err)
	}

	// Avoid deploying the same product package with multiple namespaces
	//err, info := model.DeployProductList.GetCurrentProductInfoByName(productName)
	var info *model.DeployProductListInfo

	info, err := model.DeployClusterProductRel.GetCurrentProductByProductNameClusterIdNamespace(productName, clusterId, "")
	if err == sql.ErrNoRows {
		return groupAndServices, nil
	}
	if err != nil {
		return nil, err
	}

	sc, err := schema.Unmarshal(info.Product)
	if err != nil {
		return nil, err
	}

	serviceDisplayMap := map[string]string{}
	for name, svc := range sc.Service {
		if svc.ServiceDisplay != "" {
			serviceDisplayMap[name] = svc.ServiceDisplay
		}
	}

	var lastServiceName string
	for _, info := range serviceInfoList {
		r := groupAndServices[info.Group]
		if info.ServiceName != lastServiceName {
			serviceDisplay, ok := serviceDisplayMap[info.ServiceName]
			if !ok {
				serviceDisplay = info.ServiceName
			}
			r = append(r, resultList{ServiceName: info.ServiceName, ServiceNameDisplay: serviceDisplay, ParentProductName: sc.ParentProductName})
		}
		if info.Status != model.INSTANCE_STATUS_RUNNING {
			r[len(r)-1].Alert = true
		} else if info.HealthState != model.INSTANCE_HEALTH_OK && info.HealthState != model.INSTANCE_HEALTH_NOTSET {
			r[len(r)-1].Alert = true
		}
		groupAndServices[info.Group] = r
		lastServiceName = info.ServiceName
	}
	return groupAndServices, nil
}

func getGroupBySvcName(groupAndServices map[string][]resultList, svcName string) (string, string) {
	for group, svcList := range groupAndServices {
		for _, svc := range svcList {
			if svcName == "" {
				return "", svc.ParentProductName
			}
			if svc.ServiceName == svcName {
				return group, svc.ParentProductName
			}
		}
	}
	return "default", "DTinsight"
}

func OrderDetail(ctx context.Context) apibase.Result {

	operationId := ctx.URLParam("operationId")
	clusterId, err := ctx.URLParamInt("clusterId")
	if err != nil {
		return fmt.Errorf("clusterId is empty  %v", err)
	}
	operationInfo, err := model.OperationList.GetByOperationId(operationId)
	if err != nil {
		return fmt.Errorf("OperationList GetByOperationId err %v", err)
	}
	enum, err := enums.OperationType.GetByCode(operationInfo.OperationType)
	if err != nil {
		return err
	}
	operationInfo.OperationName = enum.Desc
	operationInfo.ShowCreateTime = operationInfo.CreateTime.Time.Format(TIME_LAYOUT)

	if operationInfo.EndTime.Valid {
		operationInfo.ShowEndTime = operationInfo.EndTime.Time.Format(TIME_LAYOUT)
	}

	if operationInfo.Duration.Valid {
		operationInfo.ShowDuration = operationInfo.Duration.Float64
	} else {
		operationInfo.ShowDuration = time.Now().Sub(operationInfo.CreateTime.Time).Seconds()
	}

	resp := ChildrenResp{
		Name:          operationInfo.OperationName,
		OperationType: operationInfo.OperationType,
		ObjectValue:   operationInfo.ObjectValue,
		ObjectType:    operationInfo.ObjectType,
		Status:        operationInfo.OperationStatus,
		Desc:          operationInfo.OperationName,
		StartTime:     operationInfo.ShowCreateTime,
		EndTime:       operationInfo.ShowEndTime,
		Duration:      util.Float64OneDecimalPlaces(operationInfo.ShowDuration),
		ChildrenResp:  nil,
	}

	shellGroup, err := model.ExecShellList.SelectShellGroupByOperationId(operationId)
	if err != nil {
		return fmt.Errorf("ExecShellList SelectShellGroupByOperationId err %v", err)
	}

	if len(shellGroup) == 0 {
		return struct{}{}
	}

	for index, _ := range shellGroup {
		shellEnum, err := enums.ShellType.GetByCode(shellGroup[index].ShellType)
		if err != nil {
			return err
		}
		//err, hostInfo := model.DeployHostList.GetHostInfoBySid(shellGroup[index].Sid)
		//if err != nil {
		//	return err
		//}
		//shellGroup[index].HostIp = hostInfo.Ip
		shellGroup[index].ShellDesc = shellEnum.Desc
		shellGroup[index].ShowCreateTime = shellGroup[index].CreateTime.Time.Format(TIME_LAYOUT)
		if shellGroup[index].EndTime.Valid {
			shellGroup[index].ShowEndTime = shellGroup[index].EndTime.Time.Format(TIME_LAYOUT)
		}
		if shellGroup[index].Duration.Valid {
			shellGroup[index].ShowDuration = util.Float64OneDecimalPlaces(shellGroup[index].Duration.Float64)
		} else {
			shellGroup[index].ShowDuration = util.Float64OneDecimalPlaces(time.Now().Sub(shellGroup[index].CreateTime.Time).Seconds())
		}
	}

	if operationInfo.OperationType == enums.OperationType.HostInit.Code && len(shellGroup) == 1 {

		//hostinit  shellGroup 的长度一定是一个
		shellResp := ChildrenResp{
			Name:         shellGroup[0].ShellDesc,
			Desc:         shellGroup[0].ShellDesc,
			ExecId:       shellGroup[0].ExecId,
			HostIp:       shellGroup[0].HostIp,
			Status:       shellGroup[0].ExecStatus,
			ObjectType:   enums.OperationObjType.Host.Code,
			StartTime:    shellGroup[0].ShowCreateTime,
			EndTime:      shellGroup[0].ShowEndTime,
			Duration:     util.Float64OneDecimalPlaces(shellGroup[0].ShowDuration),
			ChildrenResp: nil,
		}
		err, _ := model.DeployHostList.GetHostInfoByIp(shellGroup[0].HostIp)
		if err == nil {
			shellResp.IsExist = true
		}
		resp.ChildrenResp = []ChildrenResp{shellResp}
		return resp
	}
	UpdateOperationStatusBySeq(int(shellGroup[0].Seq.Int32))
	services, err := getGroupAndServices(clusterId, shellGroup[0].ProductName)
	if err != nil {
		log.Errorf("getGroupAndServices error: %v", err)
	}

	//操作类型  服务 shell
	shellLearyMap := map[int]map[string][]model.ExecShellInfo{}
	for _, info := range shellGroup {
		if _, ok := shellLearyMap[info.ShellType]; !ok {
			shellLearyMap[info.ShellType] = map[string][]model.ExecShellInfo{}
		}
		shellLearyMap[info.ShellType][info.ServiceName] = append(shellLearyMap[info.ShellType][info.ServiceName], info)
	}

	var productIsExist = false
	pInfo, _ := model.DeployClusterProductRel.GetCurrentProductByProductNameClusterIdNamespace(shellGroup[0].ProductName, clusterId, "")
	if pInfo.ProductName != "" {
		productIsExist = true
	}
	for _, svcMap := range shellLearyMap {
		for svcName, shellInfos := range svcMap {
			for _, info := range shellInfos {
				var hostIsExist = false
				err, _ := model.DeployHostList.GetHostInfoByIp(info.HostIp)
				if err == nil {
					hostIsExist = true
				}
				//第一次循环为空，创建 svcLearyChildrenResp 与 shellTypeChildrenResp
				if len(resp.ChildrenResp) == 0 {
					svcLearyChildrenResp := ChildrenResp{
						Name:       info.ShellDesc,
						Desc:       info.ShellDesc,
						ShellType:  info.ShellType,
						ExecId:     info.ExecId,
						ObjectType: enums.OperationObjType.Host.Code,
						IsExist:    hostIsExist,
						HostIp:     info.HostIp,
						Status:     info.ExecStatus,
						StartTime:  info.ShowCreateTime,
						EndTime:    info.ShowEndTime,
						Duration:   util.Float64OneDecimalPlaces(info.ShowDuration),
					}
					group, parentProductName := getGroupBySvcName(services, svcName)
					shellTypeChildrenResp := ChildrenResp{
						Name:              info.ShellDesc,
						Desc:              info.ShellDesc,
						ShellType:         info.ShellType,
						ObjectValue:       svcName,
						ObjectType:        enums.OperationObjType.Svc.Code,
						Group:             group,
						ParentProductName: parentProductName,
						IsExist:           productIsExist,
						ChildrenResp:      []ChildrenResp{svcLearyChildrenResp},
					}
					resp.ChildrenResp = append(resp.ChildrenResp, shellTypeChildrenResp)
					//必须跳过本次循环后面的程序
					continue
				}
				// 不是第一次循环
				for idx, _ := range resp.ChildrenResp {
					//如果shell type 相同
					if resp.ChildrenResp[idx].ChildrenResp[0].ShellType == info.ShellType {
						//如果服务也相同 则说明 改 shell 是属于该服务下的一个操作
						if resp.ChildrenResp[idx].ObjectValue == info.ServiceName {
							resp.ChildrenResp[idx].ChildrenResp = append(resp.ChildrenResp[idx].ChildrenResp, ChildrenResp{
								Name:         info.ShellDesc,
								Desc:         info.ShellDesc,
								ShellType:    info.ShellType,
								ExecId:       info.ExecId,
								ObjectType:   enums.OperationObjType.Host.Code,
								IsExist:      hostIsExist,
								HostIp:       info.HostIp,
								Status:       info.ExecStatus,
								StartTime:    info.ShowCreateTime,
								EndTime:      info.ShowEndTime,
								Duration:     util.Float64OneDecimalPlaces(info.ShowDuration),
								ChildrenResp: nil,
							})
							break
						}

						//如果服务没找到，则说明需要添加新服务
						if idx == len(resp.ChildrenResp)-1 {
							svcLearyChildrenResp := ChildrenResp{
								Name:       info.ShellDesc,
								Desc:       info.ShellDesc,
								ShellType:  info.ShellType,
								ExecId:     info.ExecId,
								ObjectType: enums.OperationObjType.Host.Code,
								IsExist:    hostIsExist,
								HostIp:     info.HostIp,
								Status:     info.ExecStatus,
								StartTime:  info.ShowCreateTime,
								EndTime:    info.ShowEndTime,
								Duration:   util.Float64OneDecimalPlaces(info.ShowDuration),
							}
							group, parentProductName := getGroupBySvcName(services, svcName)
							shellTypeChildrenResp := ChildrenResp{
								Name:              info.ShellDesc,
								Desc:              info.ShellDesc,
								ShellType:         info.ShellType,
								ObjectValue:       svcName,
								ObjectType:        enums.OperationObjType.Svc.Code,
								Group:             group,
								ParentProductName: parentProductName,
								IsExist:           productIsExist,
								ChildrenResp:      []ChildrenResp{svcLearyChildrenResp},
							}
							resp.ChildrenResp = append(resp.ChildrenResp, shellTypeChildrenResp)
							break
						}
					}
					//如果操作类型未找到
					if idx == len(resp.ChildrenResp)-1 {
						svcLearyChildrenResp := ChildrenResp{
							Name:       info.ShellDesc,
							Desc:       info.ShellDesc,
							ShellType:  info.ShellType,
							ExecId:     info.ExecId,
							ObjectType: enums.OperationObjType.Host.Code,
							IsExist:    hostIsExist,
							HostIp:     info.HostIp,
							Status:     info.ExecStatus,
							StartTime:  info.ShowCreateTime,
							EndTime:    info.ShowEndTime,
							Duration:   util.Float64OneDecimalPlaces(info.ShowDuration),
						}
						group, parentProductName := getGroupBySvcName(services, svcName)
						shellTypeChildrenResp := ChildrenResp{
							Name:              info.ShellDesc,
							Desc:              info.ShellDesc,
							ShellType:         info.ShellType,
							ObjectValue:       svcName,
							ObjectType:        enums.OperationObjType.Svc.Code,
							Group:             group,
							ParentProductName: parentProductName,
							IsExist:           productIsExist,
							ChildrenResp:      []ChildrenResp{svcLearyChildrenResp},
						}
						resp.ChildrenResp = append(resp.ChildrenResp, shellTypeChildrenResp)
						break
					}
				}
			}
		}
	}
	for idx, _ := range resp.ChildrenResp {

		sort.Slice(resp.ChildrenResp[idx].ChildrenResp, func(i, j int) bool {
			if resp.ChildrenResp[idx].ChildrenResp[i].StartTime == resp.ChildrenResp[idx].ChildrenResp[j].StartTime {
				if resp.ChildrenResp[idx].ChildrenResp[i].ObjectValue == resp.ChildrenResp[idx].ChildrenResp[j].ObjectValue {
					return resp.ChildrenResp[idx].ChildrenResp[i].ShellType < resp.ChildrenResp[idx].ChildrenResp[j].ShellType
				}
				return resp.ChildrenResp[idx].ChildrenResp[i].ObjectValue < resp.ChildrenResp[idx].ChildrenResp[j].ObjectValue
			}
			return resp.ChildrenResp[idx].ChildrenResp[i].StartTime < resp.ChildrenResp[idx].ChildrenResp[j].StartTime
		})

		var startTimeList, endTimeList []string
		var hasRunning, hasFail bool
		for _, c := range resp.ChildrenResp[idx].ChildrenResp {
			startTimeList = append(startTimeList, c.StartTime)
			if c.EndTime != "" {
				endTimeList = append(endTimeList, c.EndTime)
			}
			if c.Status == enums.ExecStatusType.Running.Code {
				hasRunning = true
			}
			if c.Status == enums.ExecStatusType.Failed.Code {
				hasFail = true
			}
		}
		resp.ChildrenResp[idx].StartTime = getEarliestTime(startTimeList)
		resp.ChildrenResp[idx].EndTime = getLatestTime(endTimeList)
		startTime, err := time.ParseInLocation(TIME_LAYOUT, resp.ChildrenResp[idx].StartTime, time.Local)
		if err != nil {
			log.Errorf("parse startTime err: %v", err)
		}
		var endTime time.Time
		if resp.ChildrenResp[idx].EndTime != "" {
			endTime, err = time.ParseInLocation(TIME_LAYOUT, resp.ChildrenResp[idx].EndTime, time.Local)
			if err != nil {
				log.Errorf("parse endTime err: %v", err)
			}
		}
		resp.ChildrenResp[idx].Status = enums.ExecStatusType.Running.Code
		if hasFail {
			resp.ChildrenResp[idx].Status = enums.ExecStatusType.Failed.Code
		}
		if !hasFail && !hasRunning {
			resp.ChildrenResp[idx].Status = enums.ExecStatusType.Success.Code
		}

		if resp.ChildrenResp[idx].Status != enums.ExecStatusType.Running.Code {
			resp.ChildrenResp[idx].Duration = endTime.Sub(startTime).Seconds()
		} else {
			fmt.Println(time.Now())
			fmt.Println(startTime.String())
			fmt.Println(time.Now().Sub(startTime).String())
			startTime.Zone()
			resp.ChildrenResp[idx].Duration = time.Now().Sub(startTime).Seconds()
		}
		resp.ChildrenResp[idx].Duration = util.Float64OneDecimalPlaces(resp.ChildrenResp[idx].Duration)
	}

	sort.Slice(resp.ChildrenResp, func(i, j int) bool {
		if resp.ChildrenResp[i].StartTime == resp.ChildrenResp[j].StartTime {
			if resp.ChildrenResp[i].ObjectValue == resp.ChildrenResp[j].ObjectValue {
				return resp.ChildrenResp[i].ShellType < resp.ChildrenResp[j].ShellType
			}
			return resp.ChildrenResp[i].ObjectValue < resp.ChildrenResp[j].ObjectValue
		}
		return resp.ChildrenResp[i].StartTime < resp.ChildrenResp[j].StartTime
	})
	resp.ProductName = shellGroup[0].ProductName
	resp.ParentProductName = resp.ChildrenResp[0].ParentProductName
	resp.IsExist = productIsExist

	return resp
}

func getEarliestTime(timeList []string) string {
	if len(timeList) == 0 {
		return ""
	}
	earliestTime := timeList[0]
	for _, timeStr := range timeList {
		if timeStr < earliestTime {
			earliestTime = timeStr
		}
	}
	return earliestTime
}

func getLatestTime(timeList []string) string {
	if len(timeList) == 0 {
		return ""
	}
	latestTime := timeList[0]
	for _, timeStr := range timeList {
		if timeStr > latestTime {
			latestTime = timeStr
		}
	}
	return latestTime
}

func SeqReport(ctx context.Context) apibase.Result {
	log.Debugf("SeqReport: %v", ctx.Request().RequestURI)
	var reqStruct = struct {
		ExecId string `json:"execId"`
		Seq    int    `json:"seq"`
	}{}

	err := ctx.ReadJSON(&reqStruct)
	if err != nil {
		return err
	}
	log.Debugf("SeqReport parmas: %+v", reqStruct)
	return model.ExecShellList.UpdateSeqByExecId(reqStruct.ExecId, reqStruct.Seq)

}

func ShellStatusReport(ctx context.Context) apibase.Result {
	log.Debugf("ShellStatusReport: %v", ctx.Request().RequestURI)
	var reqStruct = struct {
		Seq    int `json:"seq"`
		Status int `json:"status"`
	}{}

	err := ctx.ReadJSON(&reqStruct)
	if err != nil {
		return err
	}
	log.Debugf("ShellStatusReport parmas: %+v", reqStruct)
	execShellInfo, err := model.ExecShellList.GetBySeq(reqStruct.Seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) ||
		execShellInfo.ExecStatus != enums.ExecStatusType.Running.Code ||
		//如果脚本类型为启动  不允许修改
		execShellInfo.ShellType == enums.ShellType.Start.Code {
		return nil
	}

	//如果不是运行中，表示终态 更新end_time 与 duration
	if reqStruct.Status != enums.ExecStatusType.Running.Code {
		now := time.Now()
		duration := now.Sub(execShellInfo.CreateTime.Time).Seconds()
		err = model.ExecShellList.UpdateStatusBySeq(reqStruct.Seq, reqStruct.Status, dbhelper.NullTime{Time: now, Valid: true}, sql.NullFloat64{Float64: duration, Valid: true})
		if err != nil {
			return err
		}
	}

	return UpdateOperationStatusBySeq(reqStruct.Seq)
}

func UpdateOperationStatusBySeq(seq int) error {
	shellGroup, err := model.ExecShellList.SelectShellGroupBySeq(seq)
	if err != nil {
		return err
	}
	if len(shellGroup) == 0 {
		return fmt.Errorf("未查询到该 seq 对应的 operation")
	}
	operationInfo, err := model.OperationList.GetByOperationId(shellGroup[0].OperationId)
	if err != nil {
		return err
	}
	haveRunning := false
	for idx, info := range shellGroup {
		//有一个shell失败整个 operation就是失败  瞬时值作为终态值
		if info.ExecStatus == enums.ExecStatusType.Failed.Code {
			if operationInfo.OperationStatus != enums.ExecStatusType.Failed.Code {
				now := time.Now()
				duration := now.Sub(operationInfo.CreateTime.Time).Seconds()
				return model.OperationList.UpdateStatusByOperationId(operationInfo.OperationId, enums.ExecStatusType.Failed.Code, dbhelper.NullTime{Time: now, Valid: true}, sql.NullFloat64{Float64: duration, Valid: true})
			} else {
				return nil
			}
		}

		if info.ExecStatus == enums.ExecStatusType.Running.Code {
			haveRunning = true
		}
		//success 状态
		if idx == len(shellGroup)-1 && !haveRunning {
			now := time.Now()
			duration := now.Sub(operationInfo.CreateTime.Time).Seconds()
			return model.OperationList.UpdateStatusByOperationId(info.OperationId, enums.ExecStatusType.Success.Code, dbhelper.NullTime{Time: now, Valid: true}, sql.NullFloat64{Float64: duration, Valid: true})
		}

		//running 状态
		if idx == len(shellGroup)-1 && haveRunning {
			return model.OperationList.UpdateStatusByOperationId(info.OperationId, enums.ExecStatusType.Running.Code, dbhelper.NullTime{Valid: false}, sql.NullFloat64{Valid: false})
		}
	}
	return nil
}

func IsShowLog(ctx context.Context) apibase.Result {
	//log.Debugf("IsShowLog: %v", ctx.Request().RequestURI)
	seq, err := ctx.URLParamInt("seq")
	if err != nil {
		return err
	}
	//log.Debugf("IsShowLog parmas : %d", seq)
	isExist, err := model.ExecShellList.IsExist(seq)
	if err != nil {
		return err
	}
	return isExist
}

func ShowShellLog(ctx context.Context) apibase.Result {
	log.Debugf("ShowShellLog: %v", ctx.Request().RequestURI)
	execId := ctx.URLParam("execId")

	if execId == "" {
		return fmt.Errorf("execId cannot be empty")
	}

	execInfo, err := model.ExecShellList.GetByExecId(execId)
	if err != nil {
		return fmt.Errorf("ExecShellList GetByExecId error: %v", err)
	}
	filePath := fmt.Sprintf("%s%s/%s/%d/shell.log", constant.ShellLogDir, execInfo.Sid, execInfo.CreateTime.Time.Format("2006-01-02"), execInfo.Seq.Int32)
	if !util.FileIsExist(filePath) {
		fmt.Errorf("file is not exist: %v", filePath)
	}
	log.Debugf("showShelllog filePath: %s", filePath)

	ws, err := ksocket.NewSocket(ctx)
	if err != nil {
		return err
	}

	go ksocket.SocketWriter(ws, time.Unix(0, 0), filePath)
	ksocket.SocketReader(ws)
	return nil
}

func DownLoadShellLog(ctx context.Context) apibase.Result {
	log.Debugf("DownLoadShellLog: %v", ctx.Request().RequestURI)
	execId := ctx.URLParam("execId")

	if execId == "" {
		log.Errorf("execId cannot be empty")
		return fmt.Errorf("execId cannot be empty")
	}

	execInfo, err := model.ExecShellList.GetByExecId(execId)
	if err != nil {
		log.Errorf("ExecShellList GetByExecId error: %v", err)
		return fmt.Errorf("ExecShellList GetByExecId error: %v", err)
	}
	filePath := fmt.Sprintf("%s%s/%s/%d/shell.log", constant.ShellLogDir, execInfo.Sid, execInfo.CreateTime.Time.Format("2006-01-02"), execInfo.Seq.Int32)
	if !util.FileIsExist(filePath) {
		log.Errorf("file is not existm filepath: %s", filePath)
		return fmt.Errorf("file is not existm filepath: %s", filePath)
	}
	log.Debugf("showShelllog filePath: %s", filePath)
	err = ctx.SendFile(filePath, fmt.Sprintf("%s-shell.log", execInfo.ExecId))
	if err != nil {
		return fmt.Errorf("SendFile error: %v", err)
	}
	return apibase.EmptyResult{}
}

func DownLoadShellContent(ctx context.Context) apibase.Result {
	log.Debugf("DownLoadShellLog: %v", ctx.Request().RequestURI)
	execId := ctx.URLParam("execId")

	if execId == "" {
		log.Errorf("execId cannot be empty")
		return fmt.Errorf("execId cannot be empty")
	}

	execInfo, err := model.ExecShellList.GetByExecId(execId)
	if err != nil {
		log.Errorf("ExecShellList GetByExecId error: %v", err)
		return fmt.Errorf("ExecShellList GetByExecId error: %v", err)
	}
	filePath := fmt.Sprintf("%s%s/%s/%d/content.sh", constant.ShellLogDir, execInfo.Sid, execInfo.CreateTime.Time.Format("2006-01-02"), execInfo.Seq.Int32)
	if !util.FileIsExist(filePath) {
		log.Errorf("file is not exist filepath: %s", filePath)
		return fmt.Errorf("file is not existm filepath: %s", filePath)
	}
	log.Debugf("showShelllog filePath: %s", filePath)
	err = ctx.SendFile(filePath, fmt.Sprintf("%s-content.sh", execInfo.ExecId))
	if err != nil {
		return fmt.Errorf("SendFile error: %v", err)
	}
	return apibase.EmptyResult{}
}

func PreviewShellContent(ctx context.Context) apibase.Result {
	log.Debugf("PreviewShellContent: %v", ctx.Request().RequestURI)
	execId := ctx.URLParam("execId")

	if execId == "" {
		return fmt.Errorf("execId cannot be empty")
	}

	execInfo, err := model.ExecShellList.GetByExecId(execId)
	if err != nil {
		return err
	}
	filePath := fmt.Sprintf("%s%s/%s/%d/content.sh", constant.ShellLogDir, execInfo.Sid, execInfo.CreateTime.Time.Format("2006-01-02"), execInfo.Seq.Int32)

	if !util.FileIsExist(filePath) {
		return fmt.Errorf("文件不存在 filepath: %s", filePath)
	}
	log.Debugf("showShelllog filePath: %s", filePath)

	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file err: %v", err)
	}

	return string(content)
}

func ListObjectValue(ctx context.Context) apibase.Result {
	clusterId, err := ctx.URLParamInt("clusterId")
	if err != nil {
		return fmt.Errorf("clusterId is empty")
	}
	value, err := model.OperationList.ListObjectValue(clusterId)
	if err != nil {
		return err
	}
	for idx, v := range value {
		_, err = uuid.FromString(v)
		if err == nil {
			err, info := model.DeployHostList.GetHostInfoBySid(v)
			if err != nil {
				return fmt.Errorf("DeployHostList GetHostInfoBySid error: %v", err)
			}
			value[idx] = info.Ip
		}
	}

	return value
}
