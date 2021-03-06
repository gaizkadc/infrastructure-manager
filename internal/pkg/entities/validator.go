/*
 * Copyright 2020 Nalej
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
 */

package entities

import (
	"github.com/nalej/derrors"
	"github.com/nalej/grpc-infrastructure-go"
	"github.com/nalej/grpc-installer-go"
	"github.com/nalej/grpc-organization-go"
	"github.com/nalej/grpc-provisioner-go"
	kValidation "k8s.io/apimachinery/pkg/util/validation"
	"strings"
)

const emptyRequestId = "request_id cannot be empty"
const emptyOrganizationId = "organization_id cannot be empty"
const emptyClusterId = "cluster_id cannot be empty"
const emptyNodeId = "node_id cannot be empty"

// ValidOrganizationId checks that an organization identifier has been specified.
func ValidOrganizationId(organizationID *grpc_organization_go.OrganizationId) derrors.Error {
	if organizationID.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	return nil
}

// ValidClusterId checks that an organization and cluster identifiers are present.
func ValidClusterId(clusterID *grpc_infrastructure_go.ClusterId) derrors.Error {
	if clusterID.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	if clusterID.ClusterId == "" {
		return derrors.NewInvalidArgumentError(emptyClusterId)
	}
	return nil
}

// ValidInstallRequest checks that the install request for a new cluster contains all the required
// credentials to proceeded with the installation.
func ValidInstallRequest(installRequest *grpc_installer_go.InstallRequest) derrors.Error {
	if installRequest.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	if installRequest.ClusterId != "" {
		return derrors.NewInvalidArgumentError("cluster_id must be nil, and set by this component")
	}
	if installRequest.RequestId != "" {
		return derrors.NewInvalidArgumentError("request_id must be nil, and set by this component")
	}

	authFound := false

	if installRequest.Username != "" {
		if installRequest.PrivateKey == "" {
			return derrors.NewInvalidArgumentError("expecting PrivateKey with Username")
		}
		if len(installRequest.Nodes) == 0 {
			return derrors.NewInvalidArgumentError("expecting Nodes with Username")
		}
		authFound = true
	}
	if installRequest.KubeConfigRaw != "" {
		if installRequest.Username != "" {
			return derrors.NewInvalidArgumentError("expecting KubeConfigRaw without Username")
		}
		if installRequest.PrivateKey != "" {
			return derrors.NewInvalidArgumentError("expecting KubeConfigRaw without PrivateKey")
		}
		if len(installRequest.Nodes) > 0 {
			return derrors.NewInvalidArgumentError("expecting KubeConfigRaw without Nodes")
		}
		authFound = true
	}
	if !authFound {
		return derrors.NewInvalidArgumentError("expecting KubeConfigRaw or Username, PrivateKey and Nodes")
	}
	return nil

}

// ValidRemoveClusterRequest checks that a Cluster is specified.
func ValidRemoveClusterRequest(removeClusterRequest *grpc_infrastructure_go.RemoveClusterRequest) derrors.Error {
	return derrors.NewUnimplementedError("ValidRemoveClusterRequest")
}

// ValidProvisionClusterRequest validates the request to create a new cluster.
func ValidProvisionClusterRequest(request *grpc_provisioner_go.ProvisionClusterRequest) derrors.Error {
	if request.RequestId == "" {
		return derrors.NewInvalidArgumentError("request_id must be set")
	}
	if request.IsManagementCluster {
		return derrors.NewInvalidArgumentError("you can only provision and install application clusters")
	}
	if request.OrganizationId == "" {
		return derrors.NewInvalidArgumentError("organization_id must be set")
	}
	if request.NumNodes <= 0 {
		return derrors.NewInvalidArgumentError("num_nodes must be positive")
	}
	if request.NodeType == "" {
		return derrors.NewInvalidArgumentError("node_type must be set")
	}
	if request.TargetPlatform == grpc_installer_go.Platform_AZURE && request.AzureCredentials == nil {
		return derrors.NewInvalidArgumentError("azure_credentials must be set when type is Azure")
	}
	if request.TargetPlatform == grpc_installer_go.Platform_AZURE && request.AzureOptions == nil {
		return derrors.NewInvalidArgumentError("azure_options must be set when type is Azure")
	}
	return nil
}

// ValidScaleClusterRequest checks that the scale request contains the required values.
func ValidScaleClusterRequest(request *grpc_provisioner_go.ScaleClusterRequest) derrors.Error {
	if request.RequestId != "" {
		return derrors.NewInvalidArgumentError("request_id is set by infrastructure-manager")
	}
	if request.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	if request.ClusterId == "" {
		return derrors.NewInvalidArgumentError(emptyClusterId)
	}
	if request.IsManagementCluster {
		return derrors.NewInvalidArgumentError("can only scale application clusters")
	}
	if request.TargetPlatform == grpc_installer_go.Platform_AZURE && request.AzureCredentials == nil {
		return derrors.NewInvalidArgumentError("azure_credentials cannot be empty")
	}
	if request.TargetPlatform == grpc_installer_go.Platform_AZURE && (request.AzureOptions == nil || request.AzureOptions.ResourceGroup == "") {
		return derrors.NewInvalidArgumentError("azure_options.resource_group cannot be empty")
	}
	return nil
}

// ValidUninstallClusterRequest checks that the uninstall request contains the required values.
func ValidUninstallClusterRequest(request *grpc_installer_go.UninstallClusterRequest) derrors.Error {
	if request.RequestId != "" {
		return derrors.NewInvalidArgumentError("request_id is set by infrastructure-manager")
	}
	if request.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	if request.ClusterId == "" {
		return derrors.NewInvalidArgumentError(emptyClusterId)
	}
	if request.KubeConfigRaw == "" {
		return derrors.NewInvalidArgumentError("kube_config_raw cannot be empty")
	}
	return nil
}

// ValidDecommissionClusterRequest checks that the decommission request contains the required values.
func ValidDecommissionClusterRequest(request *grpc_provisioner_go.DecommissionClusterRequest) derrors.Error {
	if request.RequestId != "" {
		return derrors.NewInvalidArgumentError("request_id is set by infrastructure-manager")
	}
	if request.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	if request.ClusterId == "" {
		return derrors.NewInvalidArgumentError(emptyClusterId)
	}
	if request.IsManagementCluster {
		return derrors.NewInvalidArgumentError("can only decommission application clusters")
	}
	if request.TargetPlatform == grpc_installer_go.Platform_AZURE && request.AzureCredentials == nil {
		return derrors.NewInvalidArgumentError("azure_credentials cannot be empty")
	}
	if request.TargetPlatform == grpc_installer_go.Platform_AZURE && (request.AzureOptions == nil || request.AzureOptions.ResourceGroup == "") {
		return derrors.NewInvalidArgumentError("azure_options.resource_group cannot be empty")
	}
	return nil
}

// ValidRemoveNodesRequest checks that the request specifies the organization and the list of nodes.
func ValidRemoveNodesRequest(removeNodesRequest *grpc_infrastructure_go.RemoveNodesRequest) derrors.Error {
	if removeNodesRequest.RequestId == "" {
		return derrors.NewInvalidArgumentError(emptyRequestId)
	}
	if removeNodesRequest.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	if len(removeNodesRequest.Nodes) == 0 {
		return derrors.NewInvalidArgumentError("nodes must not be empty")
	}
	return nil
}

// ValidLabels checks that labels conform to the Kubernetes standard.
func ValidLabels(labels map[string]string) derrors.Error {
	for _, v := range labels {
		validationErrors := kValidation.IsValidLabelValue(v)
		if len(validationErrors) != 0 {
			return derrors.NewInvalidArgumentError(strings.Join(validationErrors, ", "))
		}
	}
	return nil
}

// ValidUpdateClusterRequest validates the request for updating the information of a node. Notice that
// empty values on updateAttribute operations are not checked as the user may want those to become empty.
func ValidUpdateClusterRequest(request *grpc_infrastructure_go.UpdateClusterRequest) derrors.Error {
	if request.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	if request.ClusterId == "" {
		return derrors.NewInvalidArgumentError(emptyClusterId)
	}
	if request.AddLabels {
		validLabels := ValidLabels(request.Labels)
		if validLabels != nil {
			return validLabels
		}
	}
	return nil
}

// ValidaUpdateNodeRequest validates the request for updating the information of a node. Notice that
// empty values on updateAttribute operations are not checked as the user may want those to become empty.
func ValidUpdateNodeRequest(request *grpc_infrastructure_go.UpdateNodeRequest) derrors.Error {
	if request.OrganizationId == "" {
		return derrors.NewInvalidArgumentError(emptyOrganizationId)
	}
	if request.NodeId != "" {
		return derrors.NewInvalidArgumentError(emptyNodeId)
	}
	if request.AddLabels {
		validLabels := ValidLabels(request.Labels)
		if validLabels != nil {
			return validLabels
		}
	}
	return nil
}
