package pod

/*
   Copyright 2017 - 2020 Crunchy Data Solutions, Inc.
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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	crv1 "github.com/crunchydata/postgres-operator/apis/cr/v1"
	"github.com/crunchydata/postgres-operator/config"
	"github.com/crunchydata/postgres-operator/kubeapi"
	"github.com/crunchydata/postgres-operator/operator/backrest"
	clusteroperator "github.com/crunchydata/postgres-operator/operator/cluster"
	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// handlePostgresPodPromotion is responsible for handling updates to PG pods the occur as a result
// of a failover.  Specifically, this handler is triggered when a replica has been promoted, and
// it now has either the "promoted" or "master" role label.
func (c *Controller) handlePostgresPodPromotion(newPod *apiv1.Pod, clusterName string) error {

	if err := cleanAndCreatePostFailoverBackup(c.PodClient, c.PodClientset,
		clusterName, newPod.Namespace); err != nil {
		log.Error(err)
		return err
	}

	return nil
}

// handleStandbyPodPromotion is responsible for handling updates to PG pods the occur as a result
// of disabling standby mode.  Specifically, this handler is triggered when a standy leader
// is turned into a regular leader.
func (c *Controller) handleStandbyPromotion(newPod *apiv1.Pod, cluster crv1.Pgcluster) error {

	clusterName := cluster.Name
	namespace := cluster.Namespace

	if err := waitForStandbyPromotion(c.PodConfig, c.PodClientset, *newPod, cluster); err != nil {
		return err
	}

	// rotate the pgBouncer passwords if pgbouncer is enabled within the cluster
	if cluster.Labels[config.LABEL_PGBOUNCER] == "true" {
		parameters := map[string]string{
			config.LABEL_PGBOUNCER_ROTATE_PASSWORD: "true",
			config.LABEL_PGBOUNCER_TASK_CLUSTER:    cluster.Name,
		}
		if err := clusteroperator.CreatePgTaskforUpdatepgBouncer(c.PodClient, &cluster,
			"", parameters); err != nil {
			log.Error(err)
			return err
		}
	}

	if err := cleanAndCreatePostFailoverBackup(c.PodClient, c.PodClientset, clusterName,
		namespace); err != nil {
		log.Error(err)
		return err
	}

	return nil
}

// waitForStandbyPromotion waits for standby mode to be disabled for a specific cluster and has
// been promoted.  This is done by verifying that recovery is no longer enabled in the database,
// while also ensuring there are not any pending restarts for the database.
// done by confirming
func waitForStandbyPromotion(restConfig *rest.Config, clientset *kubernetes.Clientset, newPod apiv1.Pod,
	cluster crv1.Pgcluster) error {

	// Checks to see if the DB is in recovery
	isInRecoveryCMD := make([]string, 4)
	isInRecoveryCMD[0] = "psql"
	isInRecoveryCMD[1] = "-t"
	isInRecoveryCMD[2] = "-c"
	isInRecoveryCMD[3] = "select pg_is_in_recovery()"

	// Pulls back details about the master
	leaderStatusCMD := make([]string, 2)
	leaderStatusCMD[0] = "curl"
	leaderStatusCMD[1] = fmt.Sprintf("localhost:%s/master", config.DEFAULT_PATRONI_PORT)

	var recoveryDisabled bool

	// wait for the server to accept writes to ensure standby has truly been disabled before
	// proceeding
	duration := time.After(time.Minute * 5)
	tick := time.Tick(500 * time.Millisecond)

	for {
		select {
		case <-duration:
			return fmt.Errorf("timed out waiting for cluster %s to accept writes after disabling "+
				"standby mode", cluster.Name)
		case <-tick:
			if !recoveryDisabled {
				isInRecoveryStr, _, _ := kubeapi.ExecToPodThroughAPI(restConfig, clientset,
					isInRecoveryCMD, newPod.Spec.Containers[0].Name, newPod.Name,
					newPod.Namespace, nil)
				if strings.Contains(isInRecoveryStr, "f") {
					recoveryDisabled = true
				}
			}
			if recoveryDisabled {
				primaryJSONStr, _, _ := kubeapi.ExecToPodThroughAPI(restConfig, clientset,
					leaderStatusCMD, newPod.Spec.Containers[0].Name, newPod.Name,
					newPod.Namespace, nil)
				var primaryJSON map[string]interface{}
				json.Unmarshal([]byte(primaryJSONStr), &primaryJSON)
				if primaryJSON["state"] == "running" && (primaryJSON["pending_restart"] == nil ||
					!primaryJSON["pending_restart"].(bool)) {
					return nil
				}
			}
		}
	}
}

// cleanAndCreatePostFailoverBackup cleans up any existing backup resources and then creates
// a pgtask to trigger the creation of a post-failover backup
func cleanAndCreatePostFailoverBackup(restClient *rest.RESTClient,
	clientset *kubernetes.Clientset, clusterName, namespace string) error {

	//look up the backrest-repo pod name
	selector := fmt.Sprintf("%s=%s,%s=true", config.LABEL_PG_CLUSTER,
		clusterName, config.LABEL_PGO_BACKREST_REPO)
	pods, err := kubeapi.GetPods(clientset, selector, namespace)
	if len(pods.Items) != 1 {
		return fmt.Errorf("pods len != 1 for cluster %s", clusterName)
	} else if err != nil {
		return err
	}

	if err := backrest.CleanBackupResources(restClient, clientset, namespace,
		clusterName); err != nil {
		log.Error(err)
		return err
	}
	if _, err := backrest.CreatePostFailoverBackup(restClient, namespace,
		clusterName, pods.Items[0].Name); err != nil {
		log.Error(err)
		return err
	}

	return nil
}
