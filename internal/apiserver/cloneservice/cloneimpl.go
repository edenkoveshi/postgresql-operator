package cloneservice

/*
Copyright 2019 - 2020 Crunchy Data Solutions, Inc.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import (
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/crunchydata/postgres-operator/internal/apiserver"
	"github.com/crunchydata/postgres-operator/internal/config"
	"github.com/crunchydata/postgres-operator/internal/util"
	crv1 "github.com/crunchydata/postgres-operator/pkg/apis/crunchydata.com/v1"
	msgs "github.com/crunchydata/postgres-operator/pkg/apiservermsgs"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Clone allows a user to clone a cluster into a new deployment
func Clone(request *msgs.CloneRequest, namespace, pgouser string) msgs.CloneResponse {
	log.Debugf("clone called with ")

	// set up the response here
	response := msgs.CloneResponse{
		Status: msgs.Status{
			Code: msgs.Ok,
			Msg:  "",
		},
	}

	log.Debug("Getting pgcluster")

	// get the information about the current pgcluster by name, to ensure it
	// exists
	sourcePgcluster, err := apiserver.PGOClientset.
		CrunchydataV1().Pgclusters(namespace).
		Get(request.SourceClusterName, metav1.GetOptions{})

	// if there is an error getting the pgcluster, abort here
	if err != nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Sprintf("Could not get cluster: %s", err)
		return response
	}

	// validate the parameters of the request that do not require setting
	// additional information, so we can avoid additional API lookups
	if err := validateCloneRequest(request, *sourcePgcluster); err != nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = err.Error()
		return response
	}

	// check if the current cluster is not upgraded to the deployed
	// Operator version. If not, do not allow the command to complete
	if sourcePgcluster.Annotations[config.ANNOTATION_IS_UPGRADED] == config.ANNOTATIONS_FALSE {
		response.Status.Code = msgs.Error
		response.Status.Msg = sourcePgcluster.Name + msgs.UpgradeError
		return response
	}

	// now, let's ensure the target pgCluster does *not* exist
	if _, err := apiserver.PGOClientset.CrunchydataV1().Pgclusters(namespace).Get(request.TargetClusterName, metav1.GetOptions{}); err == nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Sprintf("Could not clone cluster: %s already exists",
			request.TargetClusterName)
		return response
	}

	// finally, let's make sure there is not already a task in progress for
	// making the clone
	selector := fmt.Sprintf("%s=true,pg-cluster=%s", config.LABEL_PGO_CLONE, request.TargetClusterName)
	taskList, err := apiserver.PGOClientset.CrunchydataV1().Pgtasks(namespace).List(metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		log.Error(err)
		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Sprintf("Could not clone cluster: could not validate %s", err.Error())
		return response
	}

	// iterate through the list of tasks and see if there are any pending
	for _, task := range taskList.Items {
		if task.Spec.Status != crv1.CompletedStatus {
			response.Status.Code = msgs.Error
			response.Status.Msg = fmt.Sprintf("Could not clone cluster: there exists an ongoing clone task: [%s]. If you believe this is an error, try deleting this pgtask CRD.", task.Spec.Name)
			return response
		}
	}

	// create the workflow task to track how this is progressing
	uid := util.RandStringBytesRmndr(4)
	workflowID, err := createWorkflowTask(request.TargetClusterName, uid, namespace)

	if err != nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Errorf("could not create clone workflow task: %s", err).Error()
		return response
	}

	// alright, begin the create the proper clone task!
	cloneTask := util.CloneTask{
		BackrestPVCSize:       request.BackrestPVCSize,
		BackrestStorageSource: request.BackrestStorageSource,
		EnableMetrics:         request.EnableMetrics,
		PGOUser:               pgouser,
		PVCSize:               request.PVCSize,
		SourceClusterName:     request.SourceClusterName,
		TargetClusterName:     request.TargetClusterName,
		TaskStepLabel:         config.LABEL_PGO_CLONE_STEP_1,
		TaskType:              crv1.PgtaskCloneStep1,
		Timestamp:             time.Now(),
		WorkflowID:            workflowID,
	}

	task := cloneTask.Create()

	// create the Pgtask CRD for the clone task
	if _, err := apiserver.PGOClientset.CrunchydataV1().Pgtasks(namespace).Create(task); err != nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Sprintf("Could not create clone task: %s", err)
		return response
	}

	response.TargetClusterName = request.TargetClusterName
	response.WorkflowID = workflowID

	return response
}

// createWorkflowTask creates the workflow task that is tracked as we attempt
// to clone the cluster
func createWorkflowTask(targetClusterName, uid, namespace string) (string, error) {
	// set a random ID for this workflow task
	u, err := ioutil.ReadFile("/proc/sys/kernel/random/uuid")

	if err != nil {
		return "", err
	}

	id := string(u[:len(u)-1])

	// set up the workflow task
	taskName := fmt.Sprintf("%s-%s-%s", targetClusterName, uid, crv1.PgtaskWorkflowCloneType)
	task := &crv1.Pgtask{
		ObjectMeta: metav1.ObjectMeta{
			Name: taskName,
			Labels: map[string]string{
				config.LABEL_PG_CLUSTER: targetClusterName,
				crv1.PgtaskWorkflowID:   id,
			},
		},
		Spec: crv1.PgtaskSpec{
			Namespace: namespace,
			Name:      taskName,
			TaskType:  crv1.PgtaskWorkflow,
			Parameters: map[string]string{
				crv1.PgtaskWorkflowSubmittedStatus: time.Now().Format(time.RFC3339),
				config.LABEL_PG_CLUSTER:            targetClusterName,
				crv1.PgtaskWorkflowID:              id,
			},
		},
	}

	// create the workflow task
	if _, err := apiserver.PGOClientset.CrunchydataV1().Pgtasks(namespace).Create(task); err != nil {
		return "", err
	}

	// return successfully after creating the task
	return id, nil
}

// validateCloneRequest validates the input from the create clone request
// that does not set any additional information
func validateCloneRequest(request *msgs.CloneRequest, cluster crv1.Pgcluster) error {
	// ensure the cluster name for the source of the clone is set
	if request.SourceClusterName == "" {
		return errors.New("the source cluster name must be set")
	}

	// ensure the cluster name for the target of the clone (the new cluster) is
	// set
	if request.TargetClusterName == "" {
		return errors.New("the target cluster name must be set")
	}

	// if any of the the PVCSizes are set to a customized value, ensure that they
	// are recognizable by Kubernetes
	// first, the primary/replica PVC size
	if err := apiserver.ValidateQuantity(request.PVCSize); err != nil {
		return fmt.Errorf(apiserver.ErrMessagePVCSize, request.PVCSize, err.Error())
	}

	// next, the pgBackRest repo PVC size
	if err := apiserver.ValidateQuantity(request.BackrestPVCSize); err != nil {
		return fmt.Errorf(apiserver.ErrMessagePVCSize, request.BackrestPVCSize, err.Error())
	}

	// clone is a form of restore, so validate using ValidateBackrestStorageTypeOnBackupRestore
	if err := util.ValidateBackrestStorageTypeOnBackupRestore(request.BackrestStorageSource,
		cluster.Spec.UserLabels[config.LABEL_BACKREST_STORAGE_TYPE], true); err != nil {
		return err
	}

	return nil
}
