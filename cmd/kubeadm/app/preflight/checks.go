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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"

	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	"k8s.io/kubernetes/pkg/api/validation"
	utilerrors "k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/initsystem"
	"k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/test/e2e_node/system"
)

type PreFlightError struct {
	Msg string
}

func (e *PreFlightError) Error() string {
	return fmt.Sprintf("[preflight] Some fatal errors occurred:\n%s%s", e.Msg, "[preflight] If you know what you are doing, you can skip pre-flight checks with `--skip-preflight-checks`")
}

// PreFlightCheck validates the state of the system to ensure kubeadm will be
// successful as often as possilble.
type PreFlightCheck interface {
	Check() (warnings, errors []error)
}

// ServiceCheck verifies that the given service is enabled and active. If we do not
// detect a supported init system however, all checks are skipped and a warning is
// returned.
type ServiceCheck struct {
	Service       string
	CheckIfActive bool
}

func (sc ServiceCheck) Check() (warnings, errors []error) {
	initSystem, err := initsystem.GetInitSystem()
	if err != nil {
		return []error{err}, nil
	}

	warnings = []error{}

	if !initSystem.ServiceExists(sc.Service) {
		warnings = append(warnings, fmt.Errorf("%s service does not exist", sc.Service))
		return warnings, nil
	}

	if !initSystem.ServiceIsEnabled(sc.Service) {
		warnings = append(warnings,
			fmt.Errorf("%s service is not enabled, please run 'systemctl enable %s.service'",
				sc.Service, sc.Service))
	}

	if sc.CheckIfActive && !initSystem.ServiceIsActive(sc.Service) {
		errors = append(errors,
			fmt.Errorf("%s service is not active, please run 'systemctl start %s.service'",
				sc.Service, sc.Service))
	}

	return warnings, errors
}

// FirewalldCheck checks if firewalld is enabled or active, and if so outputs a warning.
type FirewalldCheck struct {
	ports []int
}

func (fc FirewalldCheck) Check() (warnings, errors []error) {
	initSystem, err := initsystem.GetInitSystem()
	if err != nil {
		return []error{err}, nil
	}

	warnings = []error{}

	if !initSystem.ServiceExists("firewalld") {
		return nil, nil
	}

	if initSystem.ServiceIsActive("firewalld") {
		warnings = append(warnings,
			fmt.Errorf("firewalld is active, please ensure ports %v are open or your cluster may not function correctly",
				fc.ports))
	}

	return warnings, errors
}

// PortOpenCheck ensures the given port is available for use.
type PortOpenCheck struct {
	port int
}

func (poc PortOpenCheck) Check() (warnings, errors []error) {
	errors = []error{}
	// TODO: Get IP from KubeadmConfig
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", poc.port))
	if err != nil {
		errors = append(errors, fmt.Errorf("Port %d is in use", poc.port))
	}
	if ln != nil {
		ln.Close()
	}

	return nil, errors
}

// IsRootCheck verifies user is root
type IsRootCheck struct{}

func (irc IsRootCheck) Check() (warnings, errors []error) {
	errors = []error{}
	if os.Getuid() != 0 {
		errors = append(errors, fmt.Errorf("user is not running as root"))
	}

	return nil, errors
}

// DirAvailableCheck checks if the given directory either does not exist, or is empty.
type DirAvailableCheck struct {
	Path string
}

func (dac DirAvailableCheck) Check() (warnings, errors []error) {
	errors = []error{}
	// If it doesn't exist we are good:
	if _, err := os.Stat(dac.Path); os.IsNotExist(err) {
		return nil, nil
	}

	f, err := os.Open(dac.Path)
	if err != nil {
		errors = append(errors, fmt.Errorf("unable to check if %s is empty: %s", dac.Path, err))
		return nil, errors
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if err != io.EOF {
		errors = append(errors, fmt.Errorf("%s is not empty", dac.Path))
	}

	return nil, errors
}

// FileAvailableCheck checks that the given file does not already exist.
type FileAvailableCheck struct {
	Path string
}

func (fac FileAvailableCheck) Check() (warnings, errors []error) {
	errors = []error{}
	if _, err := os.Stat(fac.Path); err == nil {
		errors = append(errors, fmt.Errorf("%s already exists", fac.Path))
	}
	return nil, errors
}

// InPathChecks checks if the given executable is present in the path.
type InPathCheck struct {
	executable string
	mandatory  bool
}

func (ipc InPathCheck) Check() (warnings, errors []error) {
	_, err := exec.LookPath(ipc.executable)
	if err != nil {
		if ipc.mandatory {
			// Return as an error:
			return nil, []error{fmt.Errorf("%s not found in system path", ipc.executable)}
		}
		// Return as a warning:
		return []error{fmt.Errorf("%s not found in system path", ipc.executable)}, nil
	}
	return nil, nil
}

// HostnameCheck checks if hostname match dns sub domain regex.
// If hostname doesn't match this regex, kubelet will not launch static pods like kube-apiserver/kube-controller-manager and so on.
type HostnameCheck struct{}

func (hc HostnameCheck) Check() (warnings, errors []error) {
	errors = []error{}
	hostname := node.GetHostname("")
	for _, msg := range validation.ValidateNodeName(hostname, false) {
		errors = append(errors, fmt.Errorf("hostname \"%s\" %s", hostname, msg))
	}
	return nil, errors
}

// HTTPProxyCheck checks if https connection to specific host is going
// to be done directly or over proxy. If proxy detected, it will return warning.
type HTTPProxyCheck struct {
	Proto string
	Host  string
	Port  int
}

func (hst HTTPProxyCheck) Check() (warnings, errors []error) {

	url := fmt.Sprintf("%s://%s:%d", hst.Proto, hst.Host, hst.Port)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, []error{err}
	}

	proxy, err := http.DefaultTransport.(*http.Transport).Proxy(req)
	if err != nil {
		return nil, []error{err}
	}
	if proxy != nil {
		return []error{fmt.Errorf("Connection to %q uses proxy %q. If that is not intended, adjust your proxy settings", url, proxy)}, nil
	}
	return nil, nil
}

type SystemVerificationCheck struct{}

func (sysver SystemVerificationCheck) Check() (warnings, errors []error) {
	// Create a buffered writer and choose a quite large value (1M) and suppose the output from the system verification test won't exceed the limit
	// Run the system verification check, but write to out buffered writer instead of stdout
	bufw := bufio.NewWriterSize(os.Stdout, 1*1024*1024)
	reporter := &system.StreamReporter{WriteStream: bufw}

	var errs []error
	// All the validators we'd like to run:
	var validators = []system.Validator{
		&system.OSValidator{Reporter: reporter},
		&system.KernelValidator{Reporter: reporter},
		&system.CgroupsValidator{Reporter: reporter},
		&system.DockerValidator{Reporter: reporter},
	}

	// Run all validators
	for _, v := range validators {
		errs = append(errs, v.Validate(system.DefaultSysSpec))
	}

	err := utilerrors.NewAggregate(errs)
	if err != nil {
		// Only print the output from the system verification check if the check failed
		fmt.Println("[preflight] The system verification failed. Printing the output from the verification:")
		bufw.Flush()
		return nil, []error{err}
	}
	return nil, nil
}

func RunInitMasterChecks(cfg *kubeadmapi.MasterConfiguration) error {
	checks := []PreFlightCheck{
		SystemVerificationCheck{},
		IsRootCheck{},
		HostnameCheck{},
		ServiceCheck{Service: "kubelet", CheckIfActive: false},
		ServiceCheck{Service: "docker", CheckIfActive: true},
		FirewalldCheck{ports: []int{int(cfg.API.BindPort), int(cfg.Discovery.BindPort), 10250}},
		PortOpenCheck{port: int(cfg.API.BindPort)},
		PortOpenCheck{port: 8080},
		PortOpenCheck{port: int(cfg.Discovery.BindPort)},
		PortOpenCheck{port: 10250},
		PortOpenCheck{port: 10251},
		PortOpenCheck{port: 10252},
		HTTPProxyCheck{Proto: "https", Host: cfg.API.AdvertiseAddresses[0], Port: int(cfg.API.BindPort)},
		DirAvailableCheck{Path: path.Join(kubeadmapi.GlobalEnvParams.KubernetesDir, "manifests")},
		DirAvailableCheck{Path: kubeadmapi.GlobalEnvParams.HostPKIPath},
		DirAvailableCheck{Path: "/var/lib/kubelet"},
		FileAvailableCheck{Path: path.Join(kubeadmapi.GlobalEnvParams.KubernetesDir, "admin.conf")},
		FileAvailableCheck{Path: path.Join(kubeadmapi.GlobalEnvParams.KubernetesDir, "kubelet.conf")},
		InPathCheck{executable: "ip", mandatory: true},
		InPathCheck{executable: "iptables", mandatory: true},
		InPathCheck{executable: "mount", mandatory: true},
		InPathCheck{executable: "nsenter", mandatory: true},
		InPathCheck{executable: "ebtables", mandatory: false},
		InPathCheck{executable: "ethtool", mandatory: false},
		InPathCheck{executable: "socat", mandatory: false},
		InPathCheck{executable: "tc", mandatory: false},
		InPathCheck{executable: "touch", mandatory: false},
	}

	if len(cfg.Etcd.Endpoints) == 0 {
		// Only do etcd related checks when no external endpoints were specified
		checks = append(checks,
			PortOpenCheck{port: 2379},
			DirAvailableCheck{Path: "/var/lib/etcd"},
		)
	}

	return RunChecks(checks, os.Stderr)
}

func RunJoinNodeChecks(cfg *kubeadmapi.NodeConfiguration) error {
	checks := []PreFlightCheck{
		SystemVerificationCheck{},
		IsRootCheck{},
		HostnameCheck{},
		ServiceCheck{Service: "kubelet", CheckIfActive: false},
		ServiceCheck{Service: "docker", CheckIfActive: true},
		PortOpenCheck{port: 10250},
		HTTPProxyCheck{Proto: "https", Host: cfg.MasterAddresses[0], Port: int(cfg.APIPort)},
		HTTPProxyCheck{Proto: "http", Host: cfg.MasterAddresses[0], Port: int(cfg.DiscoveryPort)},
		DirAvailableCheck{Path: path.Join(kubeadmapi.GlobalEnvParams.KubernetesDir, "manifests")},
		DirAvailableCheck{Path: "/var/lib/kubelet"},
		FileAvailableCheck{Path: path.Join(kubeadmapi.GlobalEnvParams.KubernetesDir, "kubelet.conf")},
		InPathCheck{executable: "ip", mandatory: true},
		InPathCheck{executable: "iptables", mandatory: true},
		InPathCheck{executable: "mount", mandatory: true},
		InPathCheck{executable: "nsenter", mandatory: true},
		InPathCheck{executable: "ebtables", mandatory: false},
		InPathCheck{executable: "ethtool", mandatory: false},
		InPathCheck{executable: "socat", mandatory: false},
		InPathCheck{executable: "tc", mandatory: false},
		InPathCheck{executable: "touch", mandatory: false},
	}

	return RunChecks(checks, os.Stderr)
}

func RunRootCheckOnly() error {
	checks := []PreFlightCheck{
		IsRootCheck{},
	}

	return RunChecks(checks, os.Stderr)
}

// RunChecks runs each check, displays it's warnings/errors, and once all
// are processed will exit if any errors occurred.
func RunChecks(checks []PreFlightCheck, ww io.Writer) error {
	found := []error{}
	for _, c := range checks {
		warnings, errs := c.Check()
		for _, w := range warnings {
			io.WriteString(ww, fmt.Sprintf("[preflight] WARNING: %s\n", w))
		}
		for _, e := range errs {
			found = append(found, e)
		}
	}
	if len(found) > 0 {
		errs := ""
		for _, i := range found {
			errs += "\t" + i.Error() + "\n"
		}
		return &PreFlightError{Msg: errors.New(errs).Error()}
	}
	return nil
}

func TryStartKubelet() {
	// If we notice that the kubelet service is inactive, try to start it
	initSystem, err := initsystem.GetInitSystem()
	if err != nil {
		fmt.Println("[preflight] No supported init system detected, won't ensure kubelet is running.")
	} else if initSystem.ServiceExists("kubelet") && !initSystem.ServiceIsActive("kubelet") {

		fmt.Println("[preflight] Starting the kubelet service")
		if err := initSystem.ServiceStart("kubelet"); err != nil {
			fmt.Printf("[preflight] WARNING: Unable to start the kubelet service: [%v]\n", err)
			fmt.Println("[preflight] WARNING: Please ensure kubelet is running manually.")
		}
	}
}
