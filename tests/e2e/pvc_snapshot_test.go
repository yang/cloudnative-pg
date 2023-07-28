/*
Copyright The CloudNativePG Contributors

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

package e2e

import (
	"os"
	"time"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/certs"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	testUtils "github.com/cloudnative-pg/cloudnative-pg/tests/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PVC Snapshot", Label(tests.LabelSnapshot), func() {
	Context("PITR tests", Ordered, func() {
		// test env constants
		const (
			namespacePrefix = "cluster-pvc-snapshot"
			level           = tests.High
			filesDir        = fixturesDir + "/pvc_snapshot"
		)
		// minio constants
		const (
			minioCaSecName  = "minio-server-ca-secret"
			minioTLSSecName = "minio-server-tls-secret"
		)
		// file constants
		const (
			clusterToSnapshot          = filesDir + "/cluster-pvc-snapshot.yaml.template"
			clusterSnapshotRestoreFile = filesDir + "/cluster-pvc-snapshot-restore.yaml.template"
		)
		// database constants
		const (
			tableName    = "test"
			tableNameTwo = "test_two"
		)

		volumeSnapshotClassName := os.Getenv("E2E_DEFAULT_VOLUMESNAPSHOT_CLASS")

		var namespace, clusterToSnapshotName string
		BeforeAll(func() {
			if testLevelEnv.Depth < int(level) {
				Skip("Test depth is lower than the amount requested for this test")
			}

			if !IsGKE() {
				Skip("Test can run currently only on GKE")
			}

			var err error
			clusterToSnapshotName, err = env.GetResourceNameFromYAML(clusterToSnapshot)
			Expect(err).ToNot(HaveOccurred())

			namespace, err = env.CreateUniqueNamespace(namespacePrefix)
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(func() error {
				return env.DeleteNamespace(namespace)
			})

			By("creating ca and tls certificate secrets", func() {
				// create CA certificates
				_, caPair, err := testUtils.CreateSecretCA(namespace, clusterToSnapshotName, minioCaSecName, true, env)
				Expect(err).ToNot(HaveOccurred())

				// sign and create secret using CA certificate and key
				serverPair, err := caPair.CreateAndSignPair("minio-service", certs.CertTypeServer,
					[]string{"minio-service.internal.mydomain.net, minio-service.default.svc, minio-service.default,"},
				)
				Expect(err).ToNot(HaveOccurred())
				serverSecret := serverPair.GenerateCertificateSecret(namespace, minioTLSSecName)
				err = env.Client.Create(env.Ctx, serverSecret)
				Expect(err).ToNot(HaveOccurred())
			})

			By("creating the credentials for minio", func() {
				AssertStorageCredentialsAreCreated(namespace, "backup-storage-creds", "minio", "minio123")
			})

			By("setting up minio", func() {
				setup, err := testUtils.MinioSSLSetup(namespace)
				Expect(err).ToNot(HaveOccurred())
				err = testUtils.InstallMinio(env, setup, uint(testTimeouts[testUtils.MinioInstallation]))
				Expect(err).ToNot(HaveOccurred())
			})

			// Create the minio client pod and wait for it to be ready.
			// We'll use it to check if everything is archived correctly
			By("setting up minio client pod", func() {
				minioClient := testUtils.MinioSSLClient(namespace)
				err := testUtils.PodCreateAndWaitForReady(env, &minioClient, 240)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		It("correctly execute PITR with a cold snapshot", func() {
			DeferCleanup(func() error {
				return os.Unsetenv("SNAPSHOT_PITR")
			})

			By("creating the cluster to snapshot", func() {
				AssertCreateCluster(namespace, clusterToSnapshotName, clusterToSnapshot, env)
			})

			By("verify test connectivity to minio using barman-cloud-wal-archive script", func() {
				primaryPod, err := env.GetClusterPrimary(namespace, clusterToSnapshotName)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func() (bool, error) {
					connectionStatus, err := testUtils.MinioTestConnectivityUsingBarmanCloudWalArchive(
						namespace, clusterToSnapshotName, primaryPod.GetName(), "minio", "minio123")
					if err != nil {
						return false, err
					}
					return connectionStatus, nil
				}, 60).Should(BeTrue())
			})

			By("creating the snapshot", func() {
				const suffix = "test-pitr"
				err := testUtils.CreateVolumeSnapshotBackup(
					volumeSnapshotClassName,
					namespace,
					clusterToSnapshotName,
					suffix,
				)
				Expect(err).ToNot(HaveOccurred())
			})

			start := time.Now()
			var rawPITR time.Duration
			By("inserting test data and creating WALs on the cluster to be snapshotted", func() {
				AssertCreateTestData(namespace, clusterToSnapshotName, tableName, psqlClientPod)
				rawPITR = time.Since(start)
				AssertCreateTestData(namespace, clusterToSnapshotName, tableNameTwo, psqlClientPod)
				AssertArchiveWalOnMinio(namespace, clusterToSnapshotName, clusterToSnapshotName)
			})

			// create a sensible PITR
			PITR := start.Add(rawPITR).Format("2006-01-02T15:04:05")

			// pass the env variable to the template engine
			err := os.Setenv("SNAPSHOT_PITR", PITR)
			Expect(err).ToNot(HaveOccurred())

			By("creating the cluster to be restored through snapshot and PITR", func() {
				clusterToRestoreName, err := env.GetResourceNameFromYAML(clusterSnapshotRestoreFile)
				Expect(err).ToNot(HaveOccurred())
				AssertCreateCluster(namespace, clusterToRestoreName, clusterSnapshotRestoreFile, env)
				AssertClusterIsReady(namespace, clusterToRestoreName, testTimeouts[testUtils.ClusterIsReadySlow], env)
			})
		})
	})
})
