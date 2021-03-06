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

package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"github.com/nalej/derrors"
	"github.com/nalej/grpc-application-go"
	"github.com/nalej/grpc-common-go"
	"github.com/nalej/grpc-conductor-go"
	"github.com/nalej/grpc-connectivity-manager-go"
	"github.com/nalej/grpc-infrastructure-go"
	"github.com/nalej/grpc-infrastructure-manager-go"
	"github.com/nalej/grpc-installer-go"
	"github.com/nalej/grpc-organization-go"
	"github.com/nalej/grpc-provisioner-go"
	"github.com/nalej/grpc-utils/pkg/conversions"
	"github.com/nalej/infrastructure-manager/internal/pkg/bus"
	"github.com/nalej/infrastructure-manager/internal/pkg/entities"
	"github.com/nalej/infrastructure-manager/internal/pkg/monitor"
	"github.com/nalej/infrastructure-manager/internal/pkg/server/discovery/k8s"
	"github.com/rs/zerolog/log"
	"io/ioutil"
	"os"
	"time"
)

const (
	// Default timeout
	DefaultTimeout = 2 * time.Minute
	// Standard timeout for operations done in this manager
	InfrastructureManagerTimeout = time.Second * 5
)

// Manager structure with the remote clients required to coordinate infrastructure operations.
type Manager struct {
	tempPath           string
	clusterClient      grpc_infrastructure_go.ClustersClient
	nodesClient        grpc_infrastructure_go.NodesClient
	installerClient    grpc_installer_go.InstallerClient
	provisionerClient  grpc_provisioner_go.ProvisionClient
	scalerClient       grpc_provisioner_go.ScaleClient
	managementClient   grpc_provisioner_go.ManagementClient
	decommissionClient grpc_provisioner_go.DecommissionClient
	appClient          grpc_application_go.ApplicationsClient
	busManager         *bus.BusManager
}

// NewManager creates a new manager.
func NewManager(
	tempDir string,
	clusterClient grpc_infrastructure_go.ClustersClient,
	nodesClient grpc_infrastructure_go.NodesClient,
	installerClient grpc_installer_go.InstallerClient,
	provisionerClient grpc_provisioner_go.ProvisionClient,
	scalerClient grpc_provisioner_go.ScaleClient,
	managementClient grpc_provisioner_go.ManagementClient,
	decommissionClient grpc_provisioner_go.DecommissionClient,
	appClient grpc_application_go.ApplicationsClient,
	busManager *bus.BusManager) Manager {
	return Manager{
		tempPath:           tempDir,
		clusterClient:      clusterClient,
		nodesClient:        nodesClient,
		installerClient:    installerClient,
		provisionerClient:  provisionerClient,
		scalerClient:       scalerClient,
		managementClient:   managementClient,
		decommissionClient: decommissionClient,
		appClient:          appClient,
		busManager:         busManager,
	}
}

// writeTempFile writes a content to a temporal file
func (m *Manager) writeTempFile(content string, prefix string) (*string, derrors.Error) {
	tmpfile, err := ioutil.TempFile(m.tempPath, prefix)
	if err != nil {
		return nil, derrors.AsError(err, "cannot create temporal file")
	}
	_, err = tmpfile.Write([]byte(content))
	if err != nil {
		return nil, derrors.AsError(err, "cannot write temporal file")
	}
	err = tmpfile.Close()
	if err != nil {
		return nil, derrors.AsError(err, "cannot close temporal file")
	}
	tmpName := tmpfile.Name()
	return &tmpName, nil
}

func (m *Manager) attachNodes(requestID string, organizationID string, clusterID string, cluster *entities.Cluster) derrors.Error {
	for _, n := range cluster.Nodes {
		nodeToAdd := &grpc_infrastructure_go.AddNodeRequest{
			RequestId:      requestID,
			OrganizationId: organizationID,
			Ip:             n.IP,
			Labels:         n.Labels,
		}
		log.Debug().Str("IP", nodeToAdd.Ip).Msg("Adding node to SM")
		addedNode, err := m.nodesClient.AddNode(context.Background(), nodeToAdd)
		if err != nil {
			return conversions.ToDerror(err)
		}
		attachReq := &grpc_infrastructure_go.AttachNodeRequest{
			RequestId:      requestID,
			OrganizationId: organizationID,
			ClusterId:      clusterID,
			NodeId:         addedNode.NodeId,
		}
		log.Debug().Str("nodeId", attachReq.NodeId).Str("clusterID", attachReq.ClusterId).Msg("Attaching node to cluster")
		_, err = m.nodesClient.AttachNode(context.Background(), attachReq)
		if err != nil {
			return conversions.ToDerror(err)
		}
	}
	return nil
}

// addClusterToSM adds the newly discovered cluster to the system model.
func (m *Manager) addClusterToSM(requestID string, organizationID string, cluster entities.Cluster, clusterState grpc_infrastructure_go.ClusterState) (*grpc_infrastructure_go.Cluster, derrors.Error) {
	toAdd := &grpc_infrastructure_go.AddClusterRequest{
		RequestId:            requestID,
		OrganizationId:       organizationID,
		Name:                 cluster.Name,
		Hostname:             cluster.Hostname,
		ControlPlaneHostname: cluster.ControlPlaneHostname,
	}
	log.Debug().Str("name", toAdd.Name).Msg("Adding cluster to SM")
	clusterAdded, err := m.clusterClient.AddCluster(context.Background(), toAdd)
	if err != nil {
		return nil, conversions.ToDerror(err)
	}
	err = m.updateClusterState(organizationID, clusterAdded.ClusterId, clusterState)
	if err != nil {
		return nil, conversions.ToDerror(err)
	}

	// add and attach nodes
	attErr := m.attachNodes(requestID, organizationID, clusterAdded.ClusterId, &cluster)
	if attErr != nil {
		return nil, attErr
	}

	// Retrieve the cluster from system model so that it contains up-to-date information as required by the calling
	// methods. Notice that the state of the cluster may be updated on other places.
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	clusterID := &grpc_infrastructure_go.ClusterId{
		OrganizationId: clusterAdded.OrganizationId,
		ClusterId:      clusterAdded.ClusterId,
	}
	result, err := m.clusterClient.GetCluster(ctx, clusterID)
	if err != nil {
		return nil, conversions.ToDerror(err)
	}
	return result, nil
}

// removeClusterFromSM Removes the cluster entities from System Model
func (m *Manager) removeClusterFromSM(requestId string, organizationId string, clusterId string) derrors.Error {
	listNodesCtx, listNodesCancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer listNodesCancel()
	nodeList, err := m.nodesClient.ListNodes(listNodesCtx, &grpc_infrastructure_go.ClusterId{
		OrganizationId: organizationId,
		ClusterId:      clusterId,
	})
	if err != nil {
		return conversions.ToDerror(err)
	}

	if len(nodeList.Nodes) > 0 {
		removeNodesCtx, removeNodesCancel := context.WithTimeout(context.Background(), DefaultTimeout)
		nodesToRemove := make([]string, 0, len(nodeList.Nodes))
		for _, node := range nodeList.Nodes {
			nodesToRemove = append(nodesToRemove, node.NodeId)
		}
		defer removeNodesCancel()
		_, err = m.nodesClient.RemoveNodes(removeNodesCtx, &grpc_infrastructure_go.RemoveNodesRequest{
			RequestId:      requestId,
			OrganizationId: organizationId,
			Nodes:          nodesToRemove,
		})
		if err != nil {
			return conversions.ToDerror(err)
		}
	}

	removeClusterRequest := &grpc_infrastructure_go.RemoveClusterRequest{
		RequestId:      requestId,
		OrganizationId: organizationId,
		ClusterId:      clusterId,
	}

	log.Debug().
		Str("requestId", requestId).
		Str("organizationId", organizationId).
		Str("clusterId", clusterId).
		Msg("Removing cluster from SM")

	_, err = m.clusterClient.RemoveCluster(context.Background(), removeClusterRequest)
	if err != nil {
		return conversions.ToDerror(err)
	}
	return nil
}

func (m *Manager) discoverCluster(requestID string, kubeConfig string, hostname string) (*entities.Cluster, derrors.Error) {
	// Store the kubeconfig file in a temporal path.
	tempFile, err := m.writeTempFile(kubeConfig, requestID)
	defer os.Remove(*tempFile)
	if err != nil {
		return nil, err
	}
	dh := k8s.NewDiscoveryHelper(*tempFile)
	err = dh.Connect()
	if err != nil {
		return nil, err
	}
	discovered, err := dh.Discover()
	if err != nil {
		return nil, err
	}
	discovered.Hostname = hostname
	log.Debug().Str("KubernetesVersion", discovered.KubernetesVersion).
		Int("numNodes", len(discovered.Nodes)).
		Str("ControlPlaneHostname", discovered.ControlPlaneHostname).
		Str("hostname", discovered.Hostname).Msg("cluster has been discovered")

	return discovered, nil
}

// getOrCreateProvisionedCluster retrieves the target cluster from system model, or triggers the discovery of an existing cluster depending
// on the request parameters.
func (m *Manager) getOrCreateProvisionedCluster(installRequest *grpc_installer_go.InstallRequest) (*grpc_infrastructure_go.Cluster, derrors.Error) {
	var result *grpc_infrastructure_go.Cluster
	if installRequest.ClusterId == "" {
		log.Debug().Str("requestID", installRequest.RequestId).Msg("Discovering cluster")
		// Discover cluster
		discovered, err := m.discoverCluster(installRequest.RequestId, installRequest.KubeConfigRaw, installRequest.Hostname)
		if err != nil {
			return nil, err
		}
		added, err := m.addClusterToSM(installRequest.RequestId, installRequest.OrganizationId, *discovered, grpc_infrastructure_go.ClusterState_PROVISIONED)
		if err != nil {
			return nil, err
		}
		result = added
	} else {
		retrieved, err := m.getCluster(installRequest.OrganizationId, installRequest.ClusterId)
		if err != nil {
			return nil, conversions.ToDerror(err)
		}
		result = retrieved
	}
	if result == nil {
		return nil, derrors.NewInternalError("cannot discover or get existing cluster")
	}
	log.Debug().Str("clusterID", result.ClusterId).Msg("target cluster found")
	return result, nil
}

// getCluster retrieves the cluster entity from system model
func (m *Manager) getCluster(organizationID string, clusterID string) (*grpc_infrastructure_go.Cluster, derrors.Error) {
	log.Debug().Str("organizationID", organizationID).Str("clusterID", clusterID).Msg("Retrieving existing cluster")
	request := &grpc_infrastructure_go.ClusterId{
		OrganizationId: organizationID,
		ClusterId:      clusterID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	retrieved, err := m.clusterClient.GetCluster(ctx, request)
	if err != nil {
		return nil, conversions.ToDerror(err)
	}
	return retrieved, nil
}

// UpdateClusterState updates the state of a cluster in system model. The update is also sent to the bus
// so that other components of the system can react to events such as new cluster becoming available.
func (m *Manager) updateClusterState(organizationID string, clusterID string, newState grpc_infrastructure_go.ClusterState) derrors.Error {
	updateRequest := &grpc_infrastructure_go.UpdateClusterRequest{
		OrganizationId:     organizationID,
		ClusterId:          clusterID,
		UpdateClusterState: true,
		State:              newState,
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	_, err := m.clusterClient.UpdateCluster(ctx, updateRequest)
	if err != nil {
		dErr := conversions.ToDerror(err)
		log.Error().Str("trace", dErr.DebugReport()).Msg("cannot update cluster state")
		return dErr
	}

	// if correct send it to the bus
	ctxBus, cancelBus := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancelBus()
	errBus := m.busManager.SendEvents(ctxBus, updateRequest)
	if errBus != nil {
		log.Error().Err(errBus).Msg("error in the bus when sending an update cluster request")
		return errBus
	}
	return nil
}

// ProvisionAndInstallCluster provisions a new kubernetes cluster and then installs it
func (m *Manager) ProvisionAndInstallCluster(provisionRequest *grpc_provisioner_go.ProvisionClusterRequest) (*grpc_infrastructure_manager_go.ProvisionerResponse, error) {
	log.Debug().Str("organizationID", provisionRequest.OrganizationId).
		Str("platform", provisionRequest.TargetPlatform.String()).
		Str("cluster_name", provisionRequest.ClusterName).
		Msg("ProvisionAndInstallCluster")

	toAdd := entities.Cluster{
		Name:              provisionRequest.ClusterName,
		KubernetesVersion: provisionRequest.KubernetesVersion,
	}

	cluster, err := m.addClusterToSM(provisionRequest.RequestId, provisionRequest.OrganizationId, toAdd, grpc_infrastructure_go.ClusterState_PROVISIONING)
	if err != nil {
		return nil, conversions.ToGRPCError(err)
	}
	provisionRequest.ClusterId = cluster.ClusterId

	log.Debug().Str("clusterID", provisionRequest.ClusterId).Msg("provisioning cluster")
	provisionerResponse, pErr := m.provisionerClient.ProvisionCluster(context.Background(), provisionRequest)
	if pErr != nil {
		return nil, pErr
	}
	log.Debug().Str("clusterID", provisionRequest.ClusterId).Msg("cluster is being provisioned")
	provisionResponse := &grpc_infrastructure_manager_go.ProvisionerResponse{
		RequestId:      provisionerResponse.RequestId,
		OrganizationId: provisionRequest.OrganizationId,
		ClusterId:      provisionRequest.ClusterId,
		State:          provisionerResponse.State,
		Error:          provisionerResponse.Error,
	}
	mon := monitor.NewProvisionerMonitor(m.provisionerClient, m.clusterClient, *provisionResponse)
	mon.RegisterCallback(m.provisionCallback)
	go mon.LaunchMonitor()
	return provisionResponse, nil
}

// provisionCallback function that will be called once a provision operation is finished. If successful, it
// will trigger the installation of the platform.
func (m *Manager) provisionCallback(requestID string, organizationID string, clusterID string,
	lastResponse *grpc_provisioner_go.ProvisionClusterResponse, err derrors.Error) {
	log.Debug().Str("requestID", requestID).
		Str("organizationID", organizationID).Str("clusterID", clusterID).
		Msg("provisioner callback received")
	if err != nil {
		log.Error().Str("err", err.DebugReport()).Msg("error callback received")
	}
	if lastResponse == nil {
		return
	}

	newState := grpc_infrastructure_go.ClusterState_PROVISIONED
	if err != nil || lastResponse.State == grpc_provisioner_go.ProvisionProgress_ERROR {
		newState = grpc_infrastructure_go.ClusterState_FAILURE
		log.Warn().Str("requestID", requestID).Str("organizationID", organizationID).Str("clusterID", clusterID).Msg("Provision failed")
	}
	err = m.updateClusterState(organizationID, clusterID, newState)
	if err != nil {
		log.Error().Msg("unable to update cluster state after provision")
		return
	}
	if newState == grpc_infrastructure_go.ClusterState_FAILURE {
		// The provisioning operation failed, so we should not continue with the install
		log.Error().Msg("install will not be triggered as provisioning failed")
		return
	}

	discovered, err := m.discoverCluster(requestID, lastResponse.RawKubeConfig, lastResponse.Hostname)
	if err != nil {
		log.Error().Msg("unable to discover cluster")
		return
	}
	clusterUpdate := &grpc_infrastructure_go.UpdateClusterRequest{
		OrganizationId:             organizationID,
		ClusterId:                  clusterID,
		UpdateHostname:             true,
		Hostname:                   lastResponse.Hostname,
		UpdateControlPlaneHostname: true,
		ControlPlaneHostname:       discovered.ControlPlaneHostname,
	}
	_, updErr := m.UpdateCluster(clusterUpdate)
	if updErr != nil {
		log.Error().Str("trace", conversions.ToDerror(updErr).DebugReport()).Msg("error updating discovered cluster")
	}

	// create the nodes and attach the to the cluster
	attErr := m.attachNodes(requestID, organizationID, clusterID, discovered )
	if attErr != nil {
		// TODO: What to do??
		log.Error().Str("trace", attErr.DebugReport()).Msg("error attaching nodes")
	}

	installRequest := &grpc_installer_go.InstallRequest{
		RequestId:         requestID,
		OrganizationId:    organizationID,
		ClusterId:         clusterID,
		ClusterType:       grpc_infrastructure_go.ClusterType_KUBERNETES,
		InstallBaseSystem: false,
		KubeConfigRaw:     lastResponse.RawKubeConfig,
		Hostname:          lastResponse.Hostname,
		TargetPlatform:    grpc_installer_go.Platform_AZURE,
		StaticIpAddresses: lastResponse.StaticIpAddresses,
	}
	_, icErr := m.InstallCluster(installRequest)
	if icErr != nil {
		log.Error().Str("trace", conversions.ToDerror(icErr).DebugReport()).Msg("error creating install request after provisioning")
	}
	return
}

func (m *Manager) InstallCluster(request *grpc_installer_go.InstallRequest) (*grpc_common_go.OpResponse, error) {
	log.Debug().Str("organizationID", request.OrganizationId).Str("clusterID", request.ClusterId).
		Str("platform", request.TargetPlatform.String()).
		Str("hostname", request.Hostname).Msg("InstallCluster")
	cluster, err := m.getOrCreateProvisionedCluster(request)
	if err != nil {
		return nil, conversions.ToGRPCError(err)
	}
	if request.InstallBaseSystem {
		return nil, derrors.NewUnimplementedError("InstallBaseSystem not supported")
	}
	request.ClusterId = cluster.ClusterId
	if cluster.State != grpc_infrastructure_go.ClusterState_PROVISIONED {
		return nil, derrors.NewInvalidArgumentError("selected cluster is not ready for install")
	}
	err = m.updateClusterState(request.OrganizationId, request.ClusterId, grpc_infrastructure_go.ClusterState_INSTALL_IN_PROGRESS)
	if err != nil {
		log.Error().Str("trace", err.DebugReport()).Msg("cannot update cluster state")
		return nil, err
	}
	log.Debug().Str("clusterID", request.ClusterId).Msg("installing cluster")
	response, iErr := m.installerClient.InstallCluster(context.Background(), request)
	if iErr != nil {
		return nil, iErr
	}
	log.Debug().Interface("status", response.Status.String()).Msg("cluster is being installed")
	mon := monitor.NewInstallerMonitor(request.ClusterId, m.installerClient, m.clusterClient, *response)
	mon.RegisterCallback(m.installCallback)
	go mon.LaunchMonitor()
	return response, nil
}

// installCallback function called when a install operation has finished on the installer.
func (m *Manager) installCallback(
	requestID string, organizationID string, clusterID string,
	response *grpc_common_go.OpResponse, err derrors.Error) {
	log.Debug().Str("requestID", requestID).
		Str("organizationID", organizationID).Str("clusterID", clusterID).
		Msg("installer callback received for install operation")
	if err != nil {
		log.Error().Str("err", err.DebugReport()).Msg("error callback received")
	}
	if response == nil {
		return
	}

	newState := grpc_infrastructure_go.ClusterState_INSTALLED
	if err != nil || response.Status == grpc_common_go.OpStatus_FAILED {
		newState = grpc_infrastructure_go.ClusterState_FAILURE
		log.Warn().Str("requestID", requestID).Str("organizationID", organizationID).
			Str("clusterID", clusterID).Str("error", response.Error).Msg("installation failed")
	}
	err = m.updateClusterState(organizationID, clusterID, newState)
	if err != nil {
		log.Error().Msg("unable to update cluster state after install")
	}

	// Get the list of nodes, and updates the nodes.
	cID := &grpc_infrastructure_go.ClusterId{
		OrganizationId: organizationID,
		ClusterId:      clusterID,
	}
	nodes, nErr := m.nodesClient.ListNodes(context.Background(), cID)
	if nErr != nil {
		log.Error().Str("err", conversions.ToDerror(nErr).DebugReport()).Msg("cannot obtain the list of nodes in the cluster on install callback")
		return
	}

	for _, n := range nodes.Nodes {
		updateNodeRequest := &grpc_infrastructure_go.UpdateNodeRequest{
			OrganizationId: organizationID,
			NodeId:         n.NodeId,
			UpdateStatus:   true,
			Status:         n.Status,
			UpdateState:    true,
			State:          entities.OpStatusToNodeState(response.Status),
		}
		_, updateErr := m.nodesClient.UpdateNode(context.Background(), updateNodeRequest)
		if updateErr != nil {
			log.Error().Str("err", conversions.ToDerror(updateErr).DebugReport()).Msg("cannot update the node status")
			return
		}
		log.Debug().Str("organizationID", organizationID).Str("nodeId", n.NodeId).Interface("newStatus", n.Status).Msg("Node status updated")
	}
	log.Debug().Str("requestID", requestID).
		Str("organizationID", organizationID).Str("clusterID", clusterID).
		Msg("cluster has been installed")
}

// Scale the number of nodes in the cluster.
func (m *Manager) Scale(request *grpc_provisioner_go.ScaleClusterRequest) (*grpc_infrastructure_manager_go.ProvisionerResponse, derrors.Error) {
	log.Debug().Str("organizationID", request.OrganizationId).Str("clusterID", request.ClusterId).
		Str("platform", request.TargetPlatform.String()).Msg("Scale request")
	// Get the cluster and check the current state
	retrieved, err := m.getCluster(request.OrganizationId, request.ClusterId)
	if err != nil {
		return nil, err
	}
	if retrieved.State != grpc_infrastructure_go.ClusterState_INSTALLED {
		return nil, derrors.NewFailedPreconditionError("cluster should be on installed state")
	}
	// Update the state to scaling
	err = m.updateClusterState(request.OrganizationId, request.ClusterId, grpc_infrastructure_go.ClusterState_SCALING)
	if err != nil {
		return nil, err
	}
	// Send the request to the provisioner component
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	provisionerResponse, pErr := m.scalerClient.ScaleCluster(ctx, request)
	if pErr != nil {
		// Update the state to error
		err = m.updateClusterState(request.OrganizationId, request.ClusterId, grpc_infrastructure_go.ClusterState_FAILURE)
		if err != nil {
			log.Error().Str("trace", err.DebugReport()).Msg("cannot update failed cluster scale")
		}
		return nil, conversions.ToDerror(pErr)
	}
	log.Debug().Str("clusterID", request.ClusterId).Msg("cluster is being scaled")
	provisionResponse := &grpc_infrastructure_manager_go.ProvisionerResponse{
		RequestId:      provisionerResponse.RequestId,
		OrganizationId: request.OrganizationId,
		ClusterId:      request.ClusterId,
		State:          provisionerResponse.State,
		Error:          provisionerResponse.Error,
	}
	mon := monitor.NewScalerMonitor(m.scalerClient, *provisionResponse)
	mon.RegisterCallback(m.scaleCallback)
	go mon.LaunchMonitor()
	return provisionResponse, nil
}

// scaleCallback function that will be called once a provision operation is finished.
func (m *Manager) scaleCallback(requestID string, organizationID string, clusterID string,
	lastResponse *grpc_provisioner_go.ScaleClusterResponse, err derrors.Error) {
	log.Debug().Str("requestID", requestID).Msg("scaler callback received")
	if err != nil {
		log.Error().Str("err", err.DebugReport()).Msg("error callback received")
	}
	if lastResponse == nil {
		return
	}

	newState := grpc_infrastructure_go.ClusterState_INSTALLED
	if err != nil || lastResponse.State == grpc_provisioner_go.ProvisionProgress_ERROR {
		newState = grpc_infrastructure_go.ClusterState_FAILURE
		log.Warn().Str("requestID", requestID).Str("organizationID", organizationID).Str("clusterID", clusterID).Msg("Scaling failed")
	}
	err = m.updateClusterState(organizationID, clusterID, newState)
	if err != nil {
		log.Error().Msg("unable to update cluster state after scale")
	}
}

// GetCluster retrieves the cluster information.
func (m *Manager) GetCluster(clusterID *grpc_infrastructure_go.ClusterId) (*grpc_infrastructure_go.Cluster, error) {
	return m.clusterClient.GetCluster(context.Background(), clusterID)
}

// ListClusters obtains a list of the clusters in the organization.
func (m *Manager) ListClusters(organizationID *grpc_organization_go.OrganizationId) (*grpc_infrastructure_go.ClusterList, error) {
	return m.clusterClient.ListClusters(context.Background(), organizationID)
}

// UpdateCluster allows the user to update the information of a cluster.
func (m *Manager) UpdateCluster(request *grpc_infrastructure_go.UpdateClusterRequest) (*grpc_infrastructure_go.Cluster, error) {
	// update system model
	ctx, cancel := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancel()

	updateResult, updateErr := m.clusterClient.UpdateCluster(ctx, request)
	if updateErr != nil {
		return nil, updateErr
	}
	// if correct send it to the bus
	ctxBus, cancelBus := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancelBus()
	errBus := m.busManager.SendEvents(ctxBus, request)
	if errBus != nil {
		log.Error().Err(errBus).Msg("error in the bus when sending an update cluster request")
		return nil, errBus
	}
	return updateResult, nil

}

// DrainCluster reschedules the services deployed in a given cluster.
func (m *Manager) DrainCluster(clusterID *grpc_infrastructure_go.ClusterId) (*grpc_common_go.Success, error) {
	// Check this cluster is cordoned
	ctx, cancel := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancel()
	targetCluster, err := m.clusterClient.GetCluster(ctx, clusterID)
	if err != nil {
		return nil, err
	}

	log.Debug().Str("status", targetCluster.ClusterStatus.String()).Msg("cluster status")
	if targetCluster.ClusterStatus != grpc_connectivity_manager_go.ClusterStatus_OFFLINE_CORDON && targetCluster.ClusterStatus != grpc_connectivity_manager_go.ClusterStatus_ONLINE_CORDON {
		err := errors.New(fmt.Sprintf("cluster %s must be cordoned before draining", targetCluster.ClusterId))
		return nil, err
	}

	// send drain operation to the common bus
	ctxDrain, cancelDrain := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancelDrain()
	msg := &grpc_conductor_go.DrainClusterRequest{ClusterId: clusterID}
	err = m.busManager.SendOps(ctxDrain, msg)
	if err != nil {
		log.Error().Err(err).Msg("error in the bus when sending a drain cluster request")
		return nil, err
	}

	return &grpc_common_go.Success{}, nil
}

// CordonCluster blocks the deployment of new services in a given cluster.
func (m *Manager) CordonCluster(clusterID *grpc_infrastructure_go.ClusterId) (*grpc_common_go.Success, error) {
	ctx, cancel := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancel()
	succ, err := m.clusterClient.CordonCluster(ctx, clusterID)
	ctxe, cancele := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancele()
	errBus := m.busManager.SendEvents(ctxe, &grpc_infrastructure_go.SetClusterStatusRequest{ClusterId: clusterID, Cordon: true})
	if errBus != nil {
		log.Error().Err(errBus).Msg("error sending set cluster request to queue")
	}
	return succ, err
}

// CordonCluster unblocks the deployment of new services in a given cluster.
func (m *Manager) UncordonCluster(clusterID *grpc_infrastructure_go.ClusterId) (*grpc_common_go.Success, error) {
	ctx, cancel := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancel()
	succ, err := m.clusterClient.UncordonCluster(ctx, clusterID)
	ctxe, cancele := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancele()
	errBus := m.busManager.SendEvents(ctxe, &grpc_infrastructure_go.SetClusterStatusRequest{ClusterId: clusterID, Cordon: false})
	if errBus != nil {
		log.Error().Err(errBus).Msg("error sending set cluster request to queue")
	}
	return succ, err
}

// RemoveCluster removes a cluster from an organization. Notice that removing a cluster implies draining the cluster
// of running applications.
func (m *Manager) RemoveCluster(removeClusterRequest *grpc_infrastructure_go.RemoveClusterRequest) (*grpc_common_go.Success, error) {
	return nil, derrors.NewUnimplementedError("RemoveCluster is not implemented yet")
}

// UpdateNode allows the user to update the information of a node.
func (m *Manager) UpdateNode(request *grpc_infrastructure_go.UpdateNodeRequest) (*grpc_infrastructure_go.Node, error) {
	updated, err := m.nodesClient.UpdateNode(context.Background(), request)
	if err != nil {
		return nil, err
	}
	// TODO Update the labels in Kubernetes. A new proto should be added in the app cluster api to pass that information
	log.Warn().Str("organizationId", updated.OrganizationId).
		Str("nodeId", updated.NodeId).
		Str("clusterId", updated.ClusterId).
		Msg("node labels have not been updated in kubernetes")
	return updated, err
}

// ListNodes obtains a list of nodes in a cluster.
func (m *Manager) ListNodes(clusterID *grpc_infrastructure_go.ClusterId) (*grpc_infrastructure_go.NodeList, error) {
	return m.nodesClient.ListNodes(context.Background(), clusterID)
}

// RemoveNodes removes a set of nodes from the system.
func (m *Manager) RemoveNodes(removeNodesRequest *grpc_infrastructure_go.RemoveNodesRequest) (*grpc_common_go.Success, error) {
	return nil, derrors.NewUnimplementedError("RemoveNodes is not implemented yet")
}

// Uninstall proceeds to remove all Nalej created elements in the cluster.
func (m *Manager) Uninstall(request *grpc_installer_go.UninstallClusterRequest, decommissionCallback *monitor.DecommissionCallback) (*grpc_common_go.OpResponse, derrors.Error) {
	log.Debug().Str("requestID", request.RequestId).
		Str("organizationID", request.OrganizationId).Str("clusterID", request.ClusterId).
		Str("platform", request.TargetPlatform.String()).Msg("Uninstall request")
	canUninstallErr := m.canUninstallCluster(request.OrganizationId, request.ClusterId)
	if canUninstallErr != nil {
		return nil, canUninstallErr
	}
	// The cluster can be uninstalled, update its state
	err := m.updateClusterState(request.OrganizationId, request.ClusterId, grpc_infrastructure_go.ClusterState_UNINSTALLING)
	if err != nil {
		return nil, err
	}
	// Send the request to the provisioner component
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	response, uErr := m.installerClient.UninstallCluster(ctx, request)
	if uErr != nil {
		// Update the state to error
		err = m.updateClusterState(request.OrganizationId, request.ClusterId, grpc_infrastructure_go.ClusterState_FAILURE)
		if err != nil {
			log.Error().Str("trace", err.DebugReport()).Msg("cannot update failed cluster uninstall")
		}
		return nil, conversions.ToDerror(uErr)
	}
	log.Debug().Str("requestID", request.RequestId).
		Str("organizationID", request.OrganizationId).Str("clusterID", request.ClusterId).
		Msg("cluster is uninstalling")
	mon := monitor.NewInstallerMonitor(request.ClusterId, m.installerClient, m.clusterClient, *response)
	mon.RegisterCallback(m.uninstallCallback)
	mon.RegisterDecommissionCallback(decommissionCallback)
	go mon.LaunchMonitor()
	return response, nil
}

// uninstallCallback function called when an uninstall operation has finished on the installer.
func (m *Manager) uninstallCallback(
	requestID string, organizationID string, clusterID string,
	response *grpc_common_go.OpResponse, err derrors.Error) {
	log.Debug().Str("requestID", requestID).
		Str("organizationID", organizationID).Str("clusterID", clusterID).
		Msg("installer callback received for uninstall operation")
	if err != nil {
		log.Error().Str("err", err.DebugReport()).Msg("error callback received")
	}
	if response == nil {
		return
	}

	newState := grpc_infrastructure_go.ClusterState_PROVISIONED
	if err != nil || response.Status == grpc_common_go.OpStatus_FAILED {
		newState = grpc_infrastructure_go.ClusterState_FAILURE
		log.Warn().Str("requestID", requestID).Str("organizationID", organizationID).
			Str("clusterID", clusterID).Str("error", response.Error).Msg("uninstall failed")
	}
	err = m.updateClusterState(organizationID, clusterID, newState)
	if err != nil {
		log.Error().Msg("unable to update cluster state after uninstall")
	}
	log.Debug().Str("requestID", requestID).
		Str("organizationID", organizationID).Str("clusterID", clusterID).
		Msg("cluster has been uninstalled")
}

// UninstallAndDecommissionCluster frees the resources of a given cluster.
func (m *Manager) UninstallAndDecommissionCluster(request *grpc_provisioner_go.DecommissionClusterRequest) (*grpc_common_go.OpResponse, derrors.Error) {
	// Retrieve the kubeconfig from provisioner
	getKubeConfigCtx, getKubeConfigCancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer getKubeConfigCancel()
	kubeConfigResponse, err := m.managementClient.GetKubeConfig(getKubeConfigCtx, &grpc_provisioner_go.ClusterRequest{
		RequestId:           request.GetRequestId(),
		OrganizationId:      request.GetOrganizationId(),
		ClusterId:           request.GetClusterId(),
		ClusterType:         request.GetClusterType(),
		IsManagementCluster: request.GetIsManagementCluster(),
		TargetPlatform:      request.GetTargetPlatform(),
		AzureCredentials:    request.GetAzureCredentials(),
		AzureOptions:        request.GetAzureOptions(),
	})
	if err != nil {
		derr := conversions.ToDerror(err)
		log.Error().
			Err(derr).
			Str("DebugReport", derr.DebugReport()).
			Interface("request", request).
			Msg("unable to get kubeconfig from cluster")
		return nil, derr
	}
	// Trigger uninstall
	uninstallRequest := grpc_installer_go.UninstallClusterRequest{
		RequestId:      request.GetRequestId(),
		OrganizationId: request.GetOrganizationId(),
		ClusterId:      request.GetClusterId(),
		ClusterType:    request.GetClusterType(),
		KubeConfigRaw:  kubeConfigResponse.GetRawKubeConfig(),
		TargetPlatform: request.GetTargetPlatform(),
	}
	response, derr := m.Uninstall(&uninstallRequest, &monitor.DecommissionCallback{
		Callback: m.Decommission,
		Request:  request,
	})
	if derr != nil {
		log.Error().
			Err(derr).
			Str("DebugReport", derr.DebugReport()).
			Interface("request", uninstallRequest).
			Msg("unable to uninstall cluster")
		return nil, derr
	}
	return response, nil
}

func (m *Manager) Decommission(request *grpc_provisioner_go.DecommissionClusterRequest) {
	decommissionCtx, decommissionCancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer decommissionCancel()
	decommissionRequest := grpc_provisioner_go.DecommissionClusterRequest{
		RequestId:           request.GetRequestId(),
		OrganizationId:      request.GetOrganizationId(),
		ClusterId:           request.GetClusterId(),
		ClusterType:         request.GetClusterType(),
		IsManagementCluster: request.GetIsManagementCluster(),
		TargetPlatform:      request.GetTargetPlatform(),
		AzureCredentials:    request.GetAzureCredentials(),
		AzureOptions:        request.GetAzureOptions(),
	}
	decommissionerResponse, err := m.decommissionClient.DecommissionCluster(decommissionCtx, &decommissionRequest)
	if err != nil {
		derr := conversions.ToDerror(err)
		log.Error().
			Err(derr).
			Str("DebugReport", derr.DebugReport()).
			Interface("request", decommissionRequest).
			Interface("response", decommissionerResponse).
			Msg("unable to decommission cluster")
		return
	}
	mon := monitor.NewDecommissionerMonitor(m.decommissionClient, request.GetClusterId(), request.GetRequestId())
	mon.RegisterCallback(m.decommissionCallback)
	mon.LaunchMonitor()
}

func (m *Manager) decommissionCallback(clusterID string, lastResponse *grpc_common_go.OpResponse, err derrors.Error) {
	err = m.removeClusterFromSM(lastResponse.GetRequestId(), lastResponse.GetOrganizationId(), clusterID)
	if err != nil {
		log.Error().Str("err", err.DebugReport()).Msg("could not remove cluster from SM")
	}
}

// canUninstallCluster checks the current state of the cluster to confirm that an
// uninstall operation may be executed.
func (m *Manager) canUninstallCluster(organizationID string, clusterID string) derrors.Error {
	cID := &grpc_infrastructure_go.ClusterId{
		OrganizationId: organizationID,
		ClusterId:      clusterID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancel()

	cluster, err := m.clusterClient.GetCluster(ctx, cID)
	if err != nil {
		return conversions.ToDerror(err)
	}
	// Check if the cluster has applications deployed on it
	hasApps, hErr := m.clusterHasApps(organizationID, clusterID)
	if hErr != nil {
		return hErr
	}
	if hasApps {
		return derrors.NewFailedPreconditionError("target cluster has deployed applications")
	}
	if cluster.ClusterStatus != grpc_connectivity_manager_go.ClusterStatus_ONLINE_CORDON {
		return derrors.NewFailedPreconditionError("target cluster must be online and cordoned")
	}
	return nil
}

// clusterHasApps checks if any service is deployed on the given cluster.
func (m *Manager) clusterHasApps(organizationID string, clusterID string) (bool, derrors.Error) {
	ctx, cancel := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer cancel()
	orgID := &grpc_organization_go.OrganizationId{
		OrganizationId: organizationID,
	}
	instances, err := m.appClient.ListAppInstances(ctx, orgID)
	if err != nil {
		return false, conversions.ToDerror(err)
	}
	for _, inst := range instances.Instances {
		for _, sg := range inst.Groups {
			for _, s := range sg.ServiceInstances {
				if s.OrganizationId == organizationID && s.DeployedOnClusterId == clusterID {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// removeClusterNodes removes all nodes associated with a cluster.
func (m *Manager) removeClusterNodes(requestID string, organizationID string, clusterID string) derrors.Error {
	cID := &grpc_infrastructure_go.ClusterId{
		OrganizationId: organizationID,
		ClusterId:      clusterID,
	}
	// Get all nodes to obtain the ids
	listCtx, listCancel := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer listCancel()

	nodes, nErr := m.nodesClient.ListNodes(listCtx, cID)
	if nErr != nil {
		toDerr := conversions.ToDerror(nErr)
		log.Error().Str("err", toDerr.DebugReport()).Msg("cannot obtain the list of nodes in the cluster")
		return toDerr
	}

	nodeIds := make([]string, 0)
	for _, n := range nodes.Nodes {
		nodeIds = append(nodeIds, n.NodeId)
	}
	// Remove cluster nodes
	removeCtx, removeCancel := context.WithTimeout(context.Background(), InfrastructureManagerTimeout)
	defer removeCancel()

	removeRequest := &grpc_infrastructure_go.RemoveNodesRequest{
		RequestId:      requestID,
		OrganizationId: organizationID,
		Nodes:          nodeIds,
	}
	_, err := m.nodesClient.RemoveNodes(removeCtx, removeRequest)
	if err != nil {
		return conversions.ToDerror(err)
	}
	return nil
}
