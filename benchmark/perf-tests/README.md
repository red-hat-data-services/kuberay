# KubeRay Performance Tests

This directory contains a collection of large scale KubeRay tests using [clusterloader2](https://github.com/kubernetes/perf-tests/tree/master/clusterloader2).
clusterloader2 is a Kubernetes load testing tool by [SIG Scalability](https://github.com/kubernetes/community/blob/master/sig-scalability) used for Kubernetes scalability and performance testing.

## Running clusterloader2 tests

First, install the perf-tests repository and compile the clusterloader2 binary

```sh
git clone git@github.com:kubernetes/perf-tests.git
cd perf-tests/clusterloader2
go build -o clusterloader2 ./cmd
```

Run the following command to run clusterloader2 against one of the test folders. In this example we'll run the test configured in the [100-rayjob](./100-rayjob/) folder.

```sh
clusterloader2 --provider=<provider-name> --kubeconfig=<path to kubeconfig> --testconfig=100-rayjob/config.yaml
```

## Tests & Results

Each directory contains a test scenario and it's clusterloader2 configuraiton. Within the directories contains a `results` subdirectory containing junit.xml files generated by clusterloader2
for previously executed runs of the tests.

The current lists of tests are:
* [100 RayCluster test](./100-raycluster/)
* [100 RayJob test](./100-rayjob/)
* [1000 RayCluster test](./1000-raycluster/)
* [1000 RayJob test](./1000-rayjob/)
* [5000 RayCluster test](./5000-raycluster/)
* [5000 RayJob test](./5000-rayjob/)
* [10000 RayCluster test](./10000-raycluster/)
* [10000 RayJob test](./10000-rayjob/)

All published results are based on tests that ran on GKE clusters using KubeRay v1.1.1. Each test directory contains a
`results/junit.xml` file containing the Cluster Loader 2 steps that were successfully completed.
To learn more about the benchmark measurements, see [Cluster Loader 2 Measurements](https://github.com/kubernetes/perf-tests/tree/master/clusterloader2#measurement).

## Run a performance test with Kind

You can test clusterloader2 configs using Kind.

First create a kind cluster:
```sh
kind create cluster --image=kindest/node:v1.27.3
```

Install KubeRay;
```sh
helm install kuberay-operator kuberay/kuberay-operator --version 1.1.0
```

Run a clusterloader2 test:
```sh
clusterloader2 --provider kind --kubeconfig ~/.kube/config --testconfig ./100-rayjob/config.yaml
```

Note: If you want to generate a number of RayJob custom resources other than 100, you need to make the following changes: (1) modify `replicasPerNamespace` in the "Creating RayJobs" step of the config.yaml file, and (2) adjust `expect_succeeded` in the `wait-for-rayjobs.sh` file.