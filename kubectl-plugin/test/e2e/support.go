package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os/exec"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
)

const letterBytes = "abcdefghijklmnopqrstuvwxyz0123456789"

func randStringBytes(n int) string {
	// Reference: https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-go/22892986
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))] //nolint:gosec // Don't need cryptographically secure random number
	}
	return string(b)
}

func createTestNamespace() string {
	GinkgoHelper()
	suffix := randStringBytes(5)
	ns := "test-ns-" + suffix
	cmd := exec.CommandContext(context.Background(), "kubectl", "create", "namespace", ns)
	err := cmd.Run()
	Expect(err).NotTo(HaveOccurred())
	nsWithPrefix := "namespace/" + ns
	cmd = exec.CommandContext(context.Background(), "kubectl", "wait", "--timeout=20s", "--for", "jsonpath={.status.phase}=Active", nsWithPrefix)
	err = cmd.Run()
	Expect(err).NotTo(HaveOccurred())
	return ns
}

func deleteTestNamespace(ns string) {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), "kubectl", "delete", "namespace", ns)
	err := cmd.Run()
	Expect(err).NotTo(HaveOccurred())
}

func deployTestRayCluster(ns string) {
	GinkgoHelper()
	// Print current working directory
	cmd := exec.CommandContext(context.Background(), "kubectl", "apply", "-f", "../../../ray-operator/config/samples/ray-cluster.sample.yaml", "-n", ns)
	err := cmd.Run()
	Expect(err).NotTo(HaveOccurred())
	cmd = exec.CommandContext(context.Background(), "kubectl", "wait", "--timeout=300s", "--for", "jsonpath={.status.state}=ready", "raycluster/raycluster-kuberay", "-n", ns)
	err = cmd.Run()
	Expect(err).NotTo(HaveOccurred())
}

//nolint:unparam // Currently all tests use the same param; will remove the parameter once more test cases are added
func getAndCheckRayJob(
	namespace,
	name,
	expectedJobID,
	expectedJobStatus,
	expectedJobDeploymentStatus string,
) (rayjob rayv1.RayJob) {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), "kubectl", "get", "--namespace", namespace, "rayjob", name, "-o", "json")
	output, err := cmd.CombinedOutput()
	Expect(err).ToNot(HaveOccurred())

	var rayJob rayv1.RayJob
	err = json.Unmarshal(output, &rayJob)
	Expect(err).ToNot(HaveOccurred())

	Expect(rayJob.Status.JobId).To(Equal(expectedJobID))
	Expect(string(rayJob.Status.JobStatus)).To(Equal(expectedJobStatus))
	Expect(string(rayJob.Status.JobDeploymentStatus)).To(Equal(expectedJobDeploymentStatus))
	return rayJob
}

// Even though sample.yaml only has one workerGroup, we filter by groupName
// to ensure this helper remains correct if future samples define multiple groups.
func getWorkerGroupValues(ns, cluster, group string) (minReplicas, maxReplicas, replicas string) {
	GinkgoHelper()
	cmd := exec.CommandContext(context.Background(), "kubectl", "get", "raycluster", cluster, "-n", ns, "-o", "json")
	output, err := cmd.CombinedOutput()
	Expect(err).ToNot(HaveOccurred())

	var rayCluster rayv1.RayCluster
	err = json.Unmarshal(output, &rayCluster)
	Expect(err).ToNot(HaveOccurred())

	for _, wg := range rayCluster.Spec.WorkerGroupSpecs {
		if wg.GroupName != group {
			continue
		}
		return int32PtrToString(wg.MinReplicas), int32PtrToString(wg.MaxReplicas), int32PtrToString(wg.Replicas)
	}
	Fail(fmt.Sprintf("worker group %q not found in raycluster %q", group, cluster))
	return "", "", ""
}

func int32PtrToString(v *int32) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(int64(*v), 10)
}
