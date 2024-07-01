package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/internal/factory/container"
	"github.com/cri-o/cri-o/internal/storage"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"golang.org/x/sys/unix"
	types "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func (s *sandbox) InitInfraContainer(serverConfig *libconfig.Config, podContainer *storage.ContainerInfo, sandboxIDMappings *idtools.IDMappings) error {
	var err error
	s.infra, err = container.New()
	if err != nil {
		return err
	}

	// determine pause command before initializing generator
	// so a failure will not result in a half configured generator
	pauseCommand, err := PauseCommand(serverConfig, podContainer.Config)
	if err != nil {
		return err
	}

	g := s.infra.Spec()
	g.HostSpecific = true
	g.ClearProcessRlimits()

	// setup defaults for the pod sandbox
	g.SetRootReadonly(true)

	// configure default ulimits
	for _, u := range serverConfig.Ulimits() {
		g.AddProcessRlimits(u.Name, u.Hard, u.Soft)
	}
	g.SetProcessArgs(pauseCommand)

	if err := s.createResolvConf(podContainer, sandboxIDMappings); err != nil {
		return fmt.Errorf("create resolv conf: %w", err)
	}

	// Add capabilities from crio.conf if default_capabilities is defined
	if err := s.infra.SpecSetupCapabilities(&types.Capability{}, serverConfig.DefaultCapabilities, serverConfig.AddInheritableCapabilities); err != nil {
		return err
	}

	return nil
}

// Spec can only be called after a successful call to InitInfraContainer
func (s *sandbox) Spec() *generate.Generator {
	return s.infra.Spec()
}

// PauseCommand returns the pause command for the provided image configuration.
func PauseCommand(cfg *libconfig.Config, image *v1.Image) ([]string, error) {
	if cfg == nil {
		return nil, errors.New("provided configuration is nil")
	}

	// This has been explicitly set by the user, since the configuration
	// default is `/pause`
	if cfg.PauseCommand != "" {
		return []string{cfg.PauseCommand}, nil
	}
	if image == nil || (len(image.Config.Entrypoint) == 0 && len(image.Config.Cmd) == 0) {
		return nil, fmt.Errorf(
			"unable to run pause image %q: %s",
			cfg.PauseImage,
			"neither Cmd nor Entrypoint specified",
		)
	}
	cmd := []string{}
	cmd = append(cmd, image.Config.Entrypoint...)
	cmd = append(cmd, image.Config.Cmd...)
	return cmd, nil
}

func (s *sandbox) createResolvConf(podContainer *storage.ContainerInfo, sandboxIDMappings *idtools.IDMappings) (retErr error) {
	// set DNS options
	s.resolvPath = podContainer.RunDir + "/resolv.conf"

	if s.config.DnsConfig == nil {
		// Ref https://github.com/kubernetes/kubernetes/issues/120748#issuecomment-1922220911
		s.config.DnsConfig = &types.DNSConfig{}
	}

	dnsServers := s.config.DnsConfig.Servers
	dnsSearches := s.config.DnsConfig.Searches
	dnsOptions := s.config.DnsConfig.Options
	err := ParseDNSOptions(dnsServers, dnsSearches, dnsOptions, s.resolvPath)
	defer func() {
		if retErr != nil {
			if err := os.Remove(s.resolvPath); err != nil {
				retErr = fmt.Errorf("failed to remove resolvPath after failing to create it: %w", retErr)
			}
		}
	}()
	if err != nil {
		return err
	}

	if err := label.Relabel(s.resolvPath, podContainer.MountLabel, false); err != nil && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	if sandboxIDMappings != nil {
		rootPair := sandboxIDMappings.RootPair()
		if err := os.Chown(s.resolvPath, rootPair.UID, rootPair.GID); err != nil {
			return fmt.Errorf("cannot chown %s to %d:%d: %w", s.resolvPath, rootPair.UID, rootPair.GID, err)
		}
	}
	mnt := spec.Mount{
		Type:        "bind",
		Source:      s.resolvPath,
		Destination: "/etc/resolv.conf",
		Options:     []string{"ro", "bind", "nodev", "nosuid", "noexec"},
	}
	s.infra.Spec().AddMount(mnt)
	return nil
}

func ParseDNSOptions(servers, searches, options []string, path string) (retErr error) {
	nServers := len(servers)
	nSearches := len(searches)
	nOptions := len(options)
	if nServers == 0 && nSearches == 0 && nOptions == 0 {
		return copyFile("/etc/resolv.conf", path)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if nSearches > 0 {
		_, err = f.WriteString("search " + strings.Join(searches, " ") + "\n")
		if err != nil {
			return err
		}
	}

	if nServers > 0 {
		_, err = f.WriteString("nameserver " + strings.Join(servers, "\nnameserver ") + "\n")
		if err != nil {
			return err
		}
	}

	if nOptions > 0 {
		_, err = f.WriteString("options " + strings.Join(options, " ") + "\n")
		if err != nil {
			return err
		}
	}

	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
