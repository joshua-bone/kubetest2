/*
Copyright 2020 The Kubernetes Authors.

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

package deployer

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/math"
	"golang.org/x/sync/errgroup"
	"k8s.io/klog"

	"sigs.k8s.io/kubetest2/pkg/exec"
	"sigs.k8s.io/kubetest2/pkg/metadata"
)

type UpConfig struct {
	ClusterConfigs []ClusterConfig
}

type ClusterConfig struct {
	Name                    string
	Project                 string
	Autopilot               bool
	Network                 string
	MachineType             string
	NumNodes                int
	ImageType               string
	ReleaseChannel          string
	Version                 string
	WorkloadIdentityEnabled bool
	Regions                 []string
	Zones                   []string
	PrivateClusterArgs      []string
}

// Deployer implementation methods below
func (d *deployer) Up() error {
	if err := d.init(); err != nil {
		return err
	}

	defer func() {
		if d.RepoRoot == "" {
			klog.Warningf("repo-root not supplied, skip dumping cluster logs")
			return
		}
		if err := d.DumpClusterLogs(); err != nil {
			klog.Warningf("Dumping cluster logs at the end of Up() failed: %w", err)
		}
	}()

	// Only run prepare once for the first GCP project.
	if err := d.prepareGcpIfNeeded(d.projects[0]); err != nil {
		return err
	}
	if err := d.createNetwork(); err != nil {
		return err
	}

	log.Printf("creating clusters##########")

	if err := d.createClusters(); err != nil {
		return fmt.Errorf("error creating the clusters: %w", err)
	}

	if err := d.testSetup(); err != nil {
		return fmt.Errorf("error running setup for the tests: %w", err)
	}

	return nil
}

func (d *deployer) createClusters() error {
	klog.V(2).Infof("Environment: %v", os.Environ())

	totalTryCount := math.Max(len(d.regions), len(d.zones))
	for retryCount := 0; retryCount < totalTryCount; retryCount++ {
		d.retryCount = retryCount

		if err := d.createSubnets(); err != nil {
			return err
		}
		if err := d.setupNetwork(); err != nil {
			return err
		}

		eg := new(errgroup.Group)
		locationArg := locationFlag(d.regions, d.zones, retryCount)
		for i := range d.projects {
			project := d.projects[i]
			clusters := d.projectClustersLayout[project]
			subNetworkArgs := subNetworkArgs(d.autopilot, d.projects, regionFromLocation(d.regions, d.zones, d.retryCount), d.network, i)
			for j := range clusters {
				cluster := clusters[j]
				eg.Go(
					func() error {
						return d.createCluster(project, cluster, subNetworkArgs, locationArg)
					},
				)
			}
		}

		eg.Wait()
		r := retryCount
		if err := eg.Wait(); err != nil {
			if d.isRetryableError(err) {
				go func() {
					d.deleteClusters(r)
					d.deleteSubnets(r)
				}()
			} else {
				return fmt.Errorf("error creating clusters: %v", err)
			}

		} else {
			return nil
		}
	}

	return nil
}

// isRetryableError checks if the error happens during cluster creation can be potentially solved by retrying or not.
func (d *deployer) isRetryableError(err error) bool {
	for _, regx := range d.retryableErrorPatternsCompiled {
		if regx.MatchString(err.Error()) {
			return true
		}
	}
	return false
}

func (d *deployer) createCluster(project string, cluster cluster, subNetworkArgs []string, locationArg string) error {
	privateClusterArgs := privateClusterArgs(d.projects, d.network, d.privateClusterAccessLevel, d.privateClusterMasterIPRanges, cluster)
	// Create the cluster
	args := d.createCommand()
	args = append(args,
		"--project="+project,
		locationArg,
		"--network="+transformNetworkName(d.projects, d.network),
	)
	// A few args are not supported in GKE Autopilot cluster creation, so they should be left unset.
	// https://cloud.google.com/sdk/gcloud/reference/container/clusters/create-auto
	if !d.autopilot {
		args = append(args, "--machine-type="+d.machineType)
		args = append(args, "--num-nodes="+strconv.Itoa(d.nodes))
		args = append(args, "--image-type="+d.imageType)
	}

	if d.workloadIdentityEnabled {
		args = append(args, fmt.Sprintf("--workload-pool=%s.svc.id.goog", project))
	}
	if d.ReleaseChannel != "" {
		args = append(args, "--release-channel="+d.ReleaseChannel)
		if d.Version == "latest" {
			// If latest is specified, get the latest version from server config for this channel.
			actualVersion, err := resolveLatestVersionInChannel(locationArg, d.ReleaseChannel)
			if err != nil {
				return err
			}
			klog.V(0).Infof("Using the latest version %q in %q channel", actualVersion, d.ReleaseChannel)
			args = append(args, "--cluster-version="+actualVersion)
		} else {
			args = append(args, "--cluster-version="+d.Version)
		}
	} else {
		args = append(args, "--cluster-version="+d.Version)
	}
	args = append(args, subNetworkArgs...)
	args = append(args, privateClusterArgs...)
	args = append(args, cluster.name)
	output, err := runWithOutputAndBuffer(exec.Command("gcloud", args...))
	if err != nil {
		//parse output for match with regex error
		return fmt.Errorf("error creating cluster: %v, output: %q", err, output)
	}
	return nil
}

func (d *deployer) createCommand() []string {
	// Use the --create-command flag if it's explicitly specified.
	if d.createCommandFlag != "" {
		return strings.Fields(d.createCommandFlag)
	}

	fs := make([]string, 0)
	if d.gcloudCommandGroup != "" {
		fs = append(fs, d.gcloudCommandGroup)
	}
	fs = append(fs, "container", "clusters")
	if d.autopilot {
		fs = append(fs, "create-auto")
	} else {
		fs = append(fs, "create")
	}
	fs = append(fs, "--quiet")
	fs = append(fs, strings.Fields(d.gcloudExtraFlags)...)
	return fs
}

func (d *deployer) IsUp() (up bool, err error) {
	if err := d.prepareGcpIfNeeded(d.projects[0]); err != nil {
		return false, err
	}

	for _, project := range d.projects {
		for _, cluster := range d.projectClustersLayout[project] {
			if err := getClusterCredentials(project, locationFlag(d.regions, d.zones, d.retryCount), cluster.name); err != nil {
				return false, err
			}

			// naively assume that if the api server reports nodes, the cluster is up
			lines, err := exec.CombinedOutputLines(
				exec.RawCommand("kubectl get nodes -o=name"),
			)
			if err != nil {
				return false, metadata.NewJUnitError(err, strings.Join(lines, "\n"))
			}
			if len(lines) == 0 {
				return false, fmt.Errorf("project had no nodes active: %s", project)
			}
		}
	}

	return true, nil
}

func (d *deployer) testSetup() error {
	if d.testPrepared {
		// Ensure setup is a singleton.
		return nil
	}

	// Only run prepare once for the first GCP project.
	if err := d.prepareGcpIfNeeded(d.projects[0]); err != nil {
		return err
	}
	if _, err := d.Kubeconfig(); err != nil {
		return err
	}
	if err := d.getInstanceGroups(); err != nil {
		return err
	}
	if err := d.ensureFirewallRules(); err != nil {
		return err
	}
	d.testPrepared = true
	return nil
}

// Kubeconfig returns a path to a kubeconfig file for the cluster in
// a temp directory, creating one if one does not exist.
// It also sets the KUBECONFIG environment variable appropriately.
func (d *deployer) Kubeconfig() (string, error) {
	if d.kubecfgPath != "" {
		return d.kubecfgPath, nil
	}

	tmpdir, err := ioutil.TempDir("", "kubetest2-gke")
	if err != nil {
		return "", err
	}

	kubecfgFiles := make([]string, 0)
	for _, project := range d.projects {
		for _, cluster := range d.projectClustersLayout[project] {
			filename := filepath.Join(tmpdir, fmt.Sprintf("kubecfg-%s-%s", project, cluster.name))
			if err := os.Setenv("KUBECONFIG", filename); err != nil {
				return "", err
			}
			if err := getClusterCredentials(project, locationFlag(d.regions, d.zones, d.retryCount), cluster.name); err != nil {
				return "", err
			}
			kubecfgFiles = append(kubecfgFiles, filename)
		}
	}

	d.kubecfgPath = strings.Join(kubecfgFiles, string(os.PathListSeparator))
	return d.kubecfgPath, nil
}

// verifyCommonFlags validates flags for up phase.
func (d *deployer) verifyUpFlags() error {
	if len(d.projects) == 0 && d.boskosProjectsRequested <= 0 {
		return fmt.Errorf("either --project or --projects-requested with a value larger than 0 must be set for GKE deployment")
	}

	if len(d.clusters) == 0 {
		if len(d.projects) > 1 || d.boskosProjectsRequested > 1 {
			return fmt.Errorf("explicit --cluster-name must be set for multi-project profile")
		}
		if err := d.UpOptions.Validate(); err != nil {
			return err
		}
		d.clusters = generateClusterNames(d.UpOptions.NumClusters, d.commonOptions.RunID())
	} else {
		klog.V(0).Infof("explicit --cluster-name specified, ignoring --num-clusters")
	}
	if err := d.verifyNetworkFlags(); err != nil {
		return err
	}
	if err := d.verifyLocationFlags(); err != nil {
		return err
	}
	if d.nodes <= 0 {
		return fmt.Errorf("--num-nodes must be larger than 0")
	}
	if err := validateVersion(d.Version); err != nil {
		return err
	}
	return nil
}

func generateClusterNames(numClusters int, uid string) []string {
	clusters := make([]string, numClusters)
	for i := 1; i <= numClusters; i++ {
		// Naming convention: https://cloud.google.com/sdk/gcloud/reference/container/clusters/create#POSITIONAL-ARGUMENTS
		// must start with an alphabet, max length 40

		// 4 characters for kt2- prefix (short for kubetest2)
		const fixedClusterNamePrefix = "kt2-"
		// 3 characters -99 suffix
		clusterNameSuffix := strconv.Itoa(i)
		// trim the uid to only use the first 33 characters
		var id string
		if uid != "" {
			const maxIDLength = 33
			if len(uid) > maxIDLength {
				id = uid[:maxIDLength]
			} else {
				id = uid
			}
			id += "-"
		}
		clusters[i-1] = fixedClusterNamePrefix + id + clusterNameSuffix
	}
	return clusters
}

func validateVersion(version string) error {
	switch version {
	case "latest", "":
		return nil
	default:
		re, err := regexp.Compile(`(\d)\.(\d)+(\.(\d)*(.*))?`)
		if err != nil {
			return err
		}
		if !re.MatchString(version) {
			return fmt.Errorf("unknown version %q", version)
		}
	}
	return nil
}
