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

package resource

import (
	"dtstack.com/dtstack/easymatrix/matrix/k8s/constant"
	"dtstack.com/dtstack/easymatrix/matrix/model"
	modelkube "dtstack.com/dtstack/easymatrix/matrix/model/kube"
	"strconv"
)
type NamespaceListRsp struct {
	ClusterName		string 	`json:"cluster_name"`
	ClusterId		int 	`json:"cluster_id"`
	ClusterType		string 	`json:"cluster_type"`
	Namespaces		[]Namespace `json:"namespaces"`
}

type Namespace struct {
	Name 	string 	`json:"namespace"`
}
func NamespaceList(info *model.ClusterInfo) (*NamespaceListRsp,error) {
	nstbscs,err := modelkube.DeployNamespaceList.Select(strconv.Itoa(info.Id),constant.NAMESPACE_VALID,"","","")
	if err != nil{
		return nil,err
	}
	if nstbscs == nil {
		return nil,nil
	}
	nsList := []Namespace{}
	for _, tbsc := range nstbscs{
		ns := Namespace{
			Name: tbsc.Namespace,
		}
		nsList = append(nsList,ns)
	}
	return &NamespaceListRsp{
		ClusterId: info.Id,
		ClusterType: info.Type,
		ClusterName: info.Name,
		Namespaces: nsList,
	},nil
}