/*
Copyright 2016 The Kubernetes Authors.

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

package preflight

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	netutil "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	versionutil "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/klog/v2"
	system "k8s.io/system-validators/validators"
	utilsexec "k8s.io/utils/exec"
	utilsnet "k8s.io/utils/net"

	kubeadmconstants "github.com/openyurtio/openyurt/pkg/util/kubernetes/kubeadm/app/constants"
	"github.com/openyurtio/openyurt/pkg/util/kubernetes/kubeadm/app/util/initsystem"
	utilruntime "github.com/openyurtio/openyurt/pkg/util/kubernetes/kubeadm/app/util/runtime"
)

// Error defines struct for communicating error messages generated by preflight checks
type Error struct {
	Msg string
}

// Error implements the standard error interface
func (e *Error) Error() string {
	return fmt.Sprintf("[preflight] Some fatal errors occurred:\n%s%s", e.Msg, "[preflight] If you know what you are doing, you can make a check non-fatal with `--ignore-preflight-errors=...`")
}

// Preflight identifies this error as a preflight error
func (e *Error) Preflight() bool {
	return true
}

// Checker validates the state of the system to ensure kubeadm will be
// successful as often as possible.
type Checker interface {
	Check() (warnings, errorList []error)
	Name() string
}

// ContainerRuntimeCheck verifies the container runtime.
type ContainerRuntimeCheck struct {
	runtime utilruntime.ContainerRuntime
}

// Name returns label for RuntimeCheck.
func (ContainerRuntimeCheck) Name() string {
	return "CRI"
}

// Check validates the container runtime
func (crc ContainerRuntimeCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("validating the container runtime")
	if err := crc.runtime.IsRunning(); err != nil {
		errorList = append(errorList, err)
	}
	return warnings, errorList
}

// ServiceCheck verifies that the given service is enabled and active. If we do not
// detect a supported init system however, all checks are skipped and a warning is
// returned.
type ServiceCheck struct {
	Service       string
	CheckIfActive bool
	Label         string
}

// Name returns label for ServiceCheck. If not provided, will return based on the service parameter
func (sc ServiceCheck) Name() string {
	if sc.Label != "" {
		return sc.Label
	}
	return fmt.Sprintf("Service-%s", strings.Title(sc.Service))
}

// Check validates if the service is enabled and active.
func (sc ServiceCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infof("validating if the %q service is enabled and active", sc.Service)
	initSystem, err := initsystem.GetInitSystem()
	if err != nil {
		return []error{err}, nil
	}

	if !initSystem.ServiceExists(sc.Service) {
		return []error{errors.Errorf("%s service does not exist", sc.Service)}, nil
	}

	if !initSystem.ServiceIsEnabled(sc.Service) {
		warnings = append(warnings,
			errors.Errorf("%s service is not enabled, please run '%s'",
				sc.Service, initSystem.EnableCommand(sc.Service)))
	}

	if sc.CheckIfActive && !initSystem.ServiceIsActive(sc.Service) {
		errorList = append(errorList,
			errors.Errorf("%s service is not active, please run 'systemctl start %s.service'",
				sc.Service, sc.Service))
	}

	return warnings, errorList
}

// FirewalldCheck checks if firewalld is enabled or active. If it is, warn the user that there may be problems
// if no actions are taken.
type FirewalldCheck struct {
	ports []int
}

// Name returns label for FirewalldCheck.
func (FirewalldCheck) Name() string {
	return "Firewalld"
}

// Check validates if the firewall is enabled and active.
func (fc FirewalldCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("validating if the firewall is enabled and active")
	initSystem, err := initsystem.GetInitSystem()
	if err != nil {
		return []error{err}, nil
	}

	if !initSystem.ServiceExists("firewalld") {
		return nil, nil
	}

	if initSystem.ServiceIsActive("firewalld") {
		err := errors.Errorf("firewalld is active, please ensure ports %v are open or your cluster may not function correctly",
			fc.ports)
		return []error{err}, nil
	}

	return nil, nil
}

// PortOpenCheck ensures the given port is available for use.
type PortOpenCheck struct {
	port  int
	label string
}

// Name returns name for PortOpenCheck. If not known, will return "PortXXXX" based on port number
func (poc PortOpenCheck) Name() string {
	if poc.label != "" {
		return poc.label
	}
	return fmt.Sprintf("Port-%d", poc.port)
}

// Check validates if the particular port is available.
func (poc PortOpenCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infof("validating availability of port %d", poc.port)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", poc.port))
	if err != nil {
		errorList = []error{errors.Errorf("Port %d is in use", poc.port)}
	}
	if ln != nil {
		if err = ln.Close(); err != nil {
			warnings = append(warnings,
				errors.Errorf("when closing port %d, encountered %v", poc.port, err))
		}
	}

	return warnings, errorList
}

// IsPrivilegedUserCheck verifies user is privileged (linux - root, windows - Administrator)
type IsPrivilegedUserCheck struct{}

// Name returns name for IsPrivilegedUserCheck
func (IsPrivilegedUserCheck) Name() string {
	return "IsPrivilegedUser"
}

// DirAvailableCheck checks if the given directory either does not exist, or is empty.
type DirAvailableCheck struct {
	Path  string
	Label string
}

// Name returns label for individual DirAvailableChecks. If not known, will return based on path.
func (dac DirAvailableCheck) Name() string {
	if dac.Label != "" {
		return dac.Label
	}
	return fmt.Sprintf("DirAvailable-%s", strings.Replace(dac.Path, "/", "-", -1))
}

// Check validates if a directory does not exist or empty.
func (dac DirAvailableCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infof("validating the existence and emptiness of directory %s", dac.Path)

	// If it doesn't exist we are good:
	if _, err := os.Stat(dac.Path); os.IsNotExist(err) {
		return nil, nil
	}

	f, err := os.Open(dac.Path)
	if err != nil {
		return nil, []error{errors.Wrapf(err, "unable to check if %s is empty", dac.Path)}
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if err != io.EOF {
		return nil, []error{errors.Errorf("%s is not empty", dac.Path)}
	}

	return nil, nil
}

// FileAvailableCheck checks that the given file does not already exist.
type FileAvailableCheck struct {
	Path  string
	Label string
}

// Name returns label for individual FileAvailableChecks. If not known, will return based on path.
func (fac FileAvailableCheck) Name() string {
	if fac.Label != "" {
		return fac.Label
	}
	return fmt.Sprintf("FileAvailable-%s", strings.Replace(fac.Path, "/", "-", -1))
}

// Check validates if the given file does not already exist.
func (fac FileAvailableCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infof("validating the existence of file %s", fac.Path)

	if _, err := os.Stat(fac.Path); err == nil {
		return nil, []error{errors.Errorf("%s already exists", fac.Path)}
	}
	return nil, nil
}

// FileExistingCheck checks that the given file does not already exist.
type FileExistingCheck struct {
	Path  string
	Label string
}

// Name returns label for individual FileExistingChecks. If not known, will return based on path.
func (fac FileExistingCheck) Name() string {
	if fac.Label != "" {
		return fac.Label
	}
	return fmt.Sprintf("FileExisting-%s", strings.Replace(fac.Path, "/", "-", -1))
}

// Check validates if the given file already exists.
func (fac FileExistingCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infof("validating the existence of file %s", fac.Path)

	if _, err := os.Stat(fac.Path); err != nil {
		return nil, []error{errors.Errorf("%s doesn't exist", fac.Path)}
	}
	return nil, nil
}

// FileContentCheck checks that the given file contains the string Content.
type FileContentCheck struct {
	Path    string
	Content []byte
	Label   string
}

// Name returns label for individual FileContentChecks. If not known, will return based on path.
func (fcc FileContentCheck) Name() string {
	if fcc.Label != "" {
		return fcc.Label
	}
	return fmt.Sprintf("FileContent-%s", strings.Replace(fcc.Path, "/", "-", -1))
}

// Check validates if the given file contains the given content.
func (fcc FileContentCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infof("validating the contents of file %s", fcc.Path)
	f, err := os.Open(fcc.Path)
	if err != nil {
		return nil, []error{errors.Errorf("%s does not exist", fcc.Path)}
	}

	lr := io.LimitReader(f, int64(len(fcc.Content)))
	defer f.Close()

	buf := &bytes.Buffer{}
	_, err = io.Copy(buf, lr)
	if err != nil {
		return nil, []error{errors.Errorf("%s could not be read", fcc.Path)}
	}

	if !bytes.Equal(buf.Bytes(), fcc.Content) {
		return nil, []error{errors.Errorf("%s contents are not set to %s", fcc.Path, fcc.Content)}
	}
	return nil, []error{}

}

// InPathCheck checks if the given executable is present in $PATH
type InPathCheck struct {
	executable string
	mandatory  bool
	exec       utilsexec.Interface
	label      string
	suggestion string
}

// Name returns label for individual InPathCheck. If not known, will return based on path.
func (ipc InPathCheck) Name() string {
	if ipc.label != "" {
		return ipc.label
	}
	return fmt.Sprintf("FileExisting-%s", strings.Replace(ipc.executable, "/", "-", -1))
}

// Check validates if the given executable is present in the path.
func (ipc InPathCheck) Check() (warnings, errs []error) {
	klog.V(1).Infof("validating the presence of executable %s", ipc.executable)
	_, err := ipc.exec.LookPath(ipc.executable)
	if err != nil {
		if ipc.mandatory {
			// Return as an error:
			return nil, []error{errors.Errorf("%s not found in system path", ipc.executable)}
		}
		// Return as a warning:
		warningMessage := fmt.Sprintf("%s not found in system path", ipc.executable)
		if ipc.suggestion != "" {
			warningMessage += fmt.Sprintf("\nSuggestion: %s", ipc.suggestion)
		}
		return []error{errors.New(warningMessage)}, nil
	}
	return nil, nil
}

// HostnameCheck checks if hostname match dns sub domain regex.
// If hostname doesn't match this regex, kubelet will not launch static pods like kube-apiserver/kube-controller-manager and so on.
type HostnameCheck struct {
	nodeName string
}

// Name will return Hostname as name for HostnameCheck
func (HostnameCheck) Name() string {
	return "Hostname"
}

// Check validates if hostname match dns sub domain regex.
// Check hostname length and format
func (hc HostnameCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("checking whether the given node name is valid and reachable using net.LookupHost")
	for _, msg := range validation.IsQualifiedName(hc.nodeName) {
		warnings = append(warnings, errors.Errorf("invalid node name format %q: %s", hc.nodeName, msg))
	}

	addr, err := net.LookupHost(hc.nodeName)
	if addr == nil {
		warnings = append(warnings, errors.Errorf("hostname \"%s\" could not be reached", hc.nodeName))
	}
	if err != nil {
		warnings = append(warnings, errors.Wrapf(err, "hostname \"%s\"", hc.nodeName))
	}
	return warnings, errorList
}

// HTTPProxyCheck checks if https connection to specific host is going
// to be done directly or over proxy. If proxy detected, it will return warning.
type HTTPProxyCheck struct {
	Proto string
	Host  string
}

// Name returns HTTPProxy as name for HTTPProxyCheck
func (hst HTTPProxyCheck) Name() string {
	return "HTTPProxy"
}

// Check validates http connectivity type, direct or via proxy.
func (hst HTTPProxyCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("validating if the connectivity type is via proxy or direct")
	u := &url.URL{Scheme: hst.Proto, Host: hst.Host}
	if utilsnet.IsIPv6String(hst.Host) {
		u.Host = net.JoinHostPort(hst.Host, "1234")
	}

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, []error{err}
	}

	proxy, err := netutil.SetOldTransportDefaults(&http.Transport{}).Proxy(req)
	if err != nil {
		return nil, []error{err}
	}
	if proxy != nil {
		return []error{errors.Errorf("Connection to %q uses proxy %q. If that is not intended, adjust your proxy settings", u, proxy)}, nil
	}
	return nil, nil
}

// HTTPProxyCIDRCheck checks if https connection to specific subnet is going
// to be done directly or over proxy. If proxy detected, it will return warning.
// Similar to HTTPProxyCheck above, but operates with subnets and uses API
// machinery transport defaults to simulate kube-apiserver accessing cluster
// services and pods.
type HTTPProxyCIDRCheck struct {
	Proto string
	CIDR  string
}

// Name will return HTTPProxyCIDR as name for HTTPProxyCIDRCheck
func (HTTPProxyCIDRCheck) Name() string {
	return "HTTPProxyCIDR"
}

// Check validates http connectivity to first IP address in the CIDR.
// If it is not directly connected and goes via proxy it will produce warning.
func (subnet HTTPProxyCIDRCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("validating http connectivity to first IP address in the CIDR")
	if len(subnet.CIDR) == 0 {
		return nil, nil
	}

	_, cidr, err := net.ParseCIDR(subnet.CIDR)
	if err != nil {
		return nil, []error{errors.Wrapf(err, "error parsing CIDR %q", subnet.CIDR)}
	}

	testIP, err := utilsnet.GetIndexedIP(cidr, 1)
	if err != nil {
		return nil, []error{errors.Wrapf(err, "unable to get first IP address from the given CIDR (%s)", cidr.String())}
	}

	testIPstring := testIP.String()
	if len(testIP) == net.IPv6len {
		testIPstring = fmt.Sprintf("[%s]:1234", testIP)
	}
	url := fmt.Sprintf("%s://%s/", subnet.Proto, testIPstring)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, []error{err}
	}

	// Utilize same transport defaults as it will be used by API server
	proxy, err := netutil.SetOldTransportDefaults(&http.Transport{}).Proxy(req)
	if err != nil {
		return nil, []error{err}
	}
	if proxy != nil {
		return []error{errors.Errorf("connection to %q uses proxy %q. This may lead to malfunctional cluster setup. Make sure that Pod and Services IP ranges specified correctly as exceptions in proxy configuration", subnet.CIDR, proxy)}, nil
	}
	return nil, nil
}

// SystemVerificationCheck defines struct used for running the system verification node check in test/e2e_node/system
type SystemVerificationCheck struct {
	IsDocker bool
}

// Name will return SystemVerification as name for SystemVerificationCheck
func (SystemVerificationCheck) Name() string {
	return "SystemVerification"
}

// Check runs all individual checks
func (sysver SystemVerificationCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("running all checks")
	// Create a buffered writer and choose a quite large value (1M) and suppose the output from the system verification test won't exceed the limit
	// Run the system verification check, but write to out buffered writer instead of stdout
	bufw := bufio.NewWriterSize(os.Stdout, 1*1024*1024)
	reporter := &system.StreamReporter{WriteStream: bufw}

	var errs []error
	var warns []error
	// All the common validators we'd like to run:
	var validators = []system.Validator{
		&system.KernelValidator{Reporter: reporter}}

	// run the docker validator only with docker runtime
	if sysver.IsDocker {
		validators = append(validators, &system.DockerValidator{Reporter: reporter})
	}

	if runtime.GOOS == "linux" {
		//add linux validators
		validators = append(validators,
			&system.OSValidator{Reporter: reporter},
			&system.CgroupsValidator{Reporter: reporter})
	}

	// Run all validators
	for _, v := range validators {
		warn, err := v.Validate(system.DefaultSysSpec)
		if err != nil {
			errs = append(errs, err...)
		}
		if warn != nil {
			warns = append(warns, warn...)
		}
	}

	if len(errs) != 0 {
		// Only print the output from the system verification check if the check failed
		fmt.Println("[preflight] The system verification failed. Printing the output from the verification:")
		bufw.Flush()
		return warns, errs
	}
	return warns, nil
}

// KubernetesVersionCheck validates Kubernetes and kubeadm versions
type KubernetesVersionCheck struct {
	KubeadmVersion    string
	KubernetesVersion string
}

// Name will return KubernetesVersion as name for KubernetesVersionCheck
func (KubernetesVersionCheck) Name() string {
	return "KubernetesVersion"
}

// Check validates Kubernetes and kubeadm versions
func (kubever KubernetesVersionCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("validating Kubernetes and kubeadm version")
	// Skip this check for "super-custom builds", where apimachinery/the overall codebase version is not set.
	if strings.HasPrefix(kubever.KubeadmVersion, "v0.0.0") {
		return nil, nil
	}

	kadmVersion, err := versionutil.ParseSemantic(kubever.KubeadmVersion)
	if err != nil {
		return nil, []error{errors.Wrapf(err, "couldn't parse kubeadm version %q", kubever.KubeadmVersion)}
	}

	k8sVersion, err := versionutil.ParseSemantic(kubever.KubernetesVersion)
	if err != nil {
		return nil, []error{errors.Wrapf(err, "couldn't parse Kubernetes version %q", kubever.KubernetesVersion)}
	}

	// Checks if k8sVersion greater or equal than the first unsupported versions by current version of kubeadm,
	// that is major.minor+1 (all patch and pre-releases versions included)
	// NB. in semver patches number is a numeric, while prerelease is a string where numeric identifiers always have lower precedence than non-numeric identifiers.
	//     thus setting the value to x.y.0-0 we are defining the very first patch - prereleases within x.y minor release.
	firstUnsupportedVersion := versionutil.MustParseSemantic(fmt.Sprintf("%d.%d.%s", kadmVersion.Major(), kadmVersion.Minor()+1, "0-0"))
	if k8sVersion.AtLeast(firstUnsupportedVersion) {
		return []error{errors.Errorf("Kubernetes version is greater than kubeadm version. Please consider to upgrade kubeadm. Kubernetes version: %s. Kubeadm version: %d.%d.x", k8sVersion, kadmVersion.Components()[0], kadmVersion.Components()[1])}, nil
	}

	return nil, nil
}

// KubeletVersionCheck validates installed kubelet version
type KubeletVersionCheck struct {
	KubernetesVersion string
	exec              utilsexec.Interface
}

// Name will return KubeletVersion as name for KubeletVersionCheck
func (KubeletVersionCheck) Name() string {
	return "KubeletVersion"
}

// Check validates kubelet version. It should be not less than minimal supported version
func (kubever KubeletVersionCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("validating kubelet version")
	kubeletVersion, err := GetKubeletVersion(kubever.exec)
	if err != nil {
		return nil, []error{errors.Wrap(err, "couldn't get kubelet version")}
	}
	if kubeletVersion.LessThan(kubeadmconstants.MinimumKubeletVersion) {
		return nil, []error{errors.Errorf("Kubelet version %q is lower than kubeadm can support. Please upgrade kubelet", kubeletVersion)}
	}

	if kubever.KubernetesVersion != "" {
		k8sVersion, err := versionutil.ParseSemantic(kubever.KubernetesVersion)
		if err != nil {
			return nil, []error{errors.Wrapf(err, "couldn't parse Kubernetes version %q", kubever.KubernetesVersion)}
		}
		if kubeletVersion.Major() > k8sVersion.Major() || kubeletVersion.Minor() > k8sVersion.Minor() {
			return nil, []error{errors.Errorf("the kubelet version is higher than the control plane version. This is not a supported version skew and may lead to a malfunctional cluster. Kubelet version: %q Control plane version: %q", kubeletVersion, k8sVersion)}
		}
	}
	return nil, nil
}

// SwapCheck warns if swap is enabled
type SwapCheck struct{}

// Name will return Swap as name for SwapCheck
func (SwapCheck) Name() string {
	return "Swap"
}

// Check validates whether swap is enabled or not
func (swc SwapCheck) Check() (warnings, errorList []error) {
	klog.V(1).Infoln("validating whether swap is enabled or not")
	f, err := os.Open("/proc/swaps")
	if err != nil {
		// /proc/swaps not available, thus no reasons to warn
		return nil, nil
	}
	defer f.Close()
	var buf []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		buf = append(buf, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, []error{errors.Wrap(err, "error parsing /proc/swaps")}
	}

	if len(buf) > 1 {
		return nil, []error{errors.New("running with swap on is not supported. Please disable swap")}
	}

	return nil, nil
}

// ImagePullCheck will pull container images used by kubeadm
type ImagePullCheck struct {
	runtime         utilruntime.ContainerRuntime
	imageList       []string
	imagePullPolicy v1.PullPolicy
}

// Name returns the label for ImagePullCheck
func (ImagePullCheck) Name() string {
	return "ImagePull"
}

// Check pulls images required by kubeadm. This is a mutating check
func (ipc ImagePullCheck) Check() (warnings, errorList []error) {
	policy := ipc.imagePullPolicy
	klog.V(1).Infof("using image pull policy: %s", policy)
	for _, image := range ipc.imageList {
		switch policy {
		case v1.PullNever:
			klog.V(1).Infof("skipping pull of image: %s", image)
			continue
		case v1.PullIfNotPresent:
			ret, err := ipc.runtime.ImageExists(image)
			if ret && err == nil {
				klog.V(1).Infof("image exists: %s", image)
				continue
			}
			if err != nil {
				errorList = append(errorList, errors.Wrapf(err, "failed to check if image %s exists", image))
			}
			fallthrough // Proceed with pulling the image if it does not exist
		case v1.PullAlways:
			klog.V(1).Infof("pulling: %s", image)
			if err := ipc.runtime.PullImage(image); err != nil {
				errorList = append(errorList, errors.Wrapf(err, "failed to pull image %s", image))
			}
		default:
			// If the policy is unknown return early with an error
			errorList = append(errorList, errors.Errorf("unsupported pull policy %q", policy))
			return warnings, errorList
		}
	}
	return warnings, errorList
}

// NumCPUCheck checks if current number of CPUs is not less than required
type NumCPUCheck struct {
	NumCPU int
}

// Name returns the label for NumCPUCheck
func (NumCPUCheck) Name() string {
	return "NumCPU"
}

// Check number of CPUs required by kubeadm
func (ncc NumCPUCheck) Check() (warnings, errorList []error) {
	numCPU := runtime.NumCPU()
	if numCPU < ncc.NumCPU {
		errorList = append(errorList, errors.Errorf("the number of available CPUs %d is less than the required %d", numCPU, ncc.NumCPU))
	}
	return warnings, errorList
}

// RunRootCheckOnly initializes checks slice of structs and call RunChecks
func RunRootCheckOnly(ignorePreflightErrors sets.String) error {
	checks := []Checker{
		IsPrivilegedUserCheck{},
	}

	return RunChecks(checks, os.Stderr, ignorePreflightErrors)
}

// RunChecks runs each check, displays it's warnings/errors, and once all
// are processed will exit if any errors occurred.
func RunChecks(checks []Checker, ww io.Writer, ignorePreflightErrors sets.String) error {
	var errsBuffer bytes.Buffer

	for _, c := range checks {
		name := c.Name()
		warnings, errs := c.Check()

		if setHasItemOrAll(ignorePreflightErrors, name) {
			// Decrease severity of errors to warnings for this check
			warnings = append(warnings, errs...)
			errs = []error{}
		}

		for _, w := range warnings {
			io.WriteString(ww, fmt.Sprintf("\t[WARNING %s]: %v\n", name, w))
		}
		for _, i := range errs {
			errsBuffer.WriteString(fmt.Sprintf("\t[ERROR %s]: %v\n", name, i.Error()))
		}
	}
	if errsBuffer.Len() > 0 {
		return &Error{Msg: errsBuffer.String()}
	}
	return nil
}

// setHasItemOrAll is helper function that return true if item is present in the set (case insensitive) or special key 'all' is present
func setHasItemOrAll(s sets.String, item string) bool {
	if s.Has("all") || s.Has(strings.ToLower(item)) {
		return true
	}
	return false
}
