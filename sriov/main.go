package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/intel/sriov-cni/pkg/config"
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func cmdAdd(args *skel.CmdArgs) error {
	n, err := config.LoadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("SRIOV-CNI failed to load netconf: %v", err)
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	if n.IF0NAME != "" {
		args.IfName = n.IF0NAME
	}

	// Try assigning a VF from PF
	if n.DeviceInfo == nil && n.Master != "" {
		// Populate device info from PF
		if err := config.AssignFreeVF(n); err != nil {
			return fmt.Errorf("unable to get VF information %+v", err)
		}
	}

	if n.Sharedvf && !n.L2Mode {
		return fmt.Errorf("l2enable mode must be true to use shared net interface %q", n.Master)
	}

	// fill in DpdkConf from DeviceInfo
	if n.DPDKMode {
		n.DPDKConf.PCIaddr = n.DeviceInfo.PCIaddr
		n.DPDKConf.Ifname = args.IfName
		n.DPDKConf.VFID = n.DeviceInfo.Vfid
	}

	if n.DeviceInfo != nil && n.DeviceInfo.PCIaddr != "" && n.DeviceInfo.Vfid >= 0 && n.DeviceInfo.Pfname != "" {
		if err = setupVF(n, args.IfName, args.ContainerID, netns); err != nil {
			return fmt.Errorf("failed to set up pod interface %q from the device %q: %v", args.IfName, n.Master, err)
		}
	} else {
		return fmt.Errorf("VF information are not available to invoke setupVF()")
	}

	// skip the IPAM allocation for L2 mode
	var result *types.Result
	if n.L2Mode {
		return result.Print()
	}

	// experimental: run IPAM allocation for DPDK mode
	if n.DPDKMode && n.IPAM.Type != "" {
		result, err = ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			return fmt.Errorf("failed to set up IPAM plugin type %q from the device %q: %v", n.IPAM.Type, n.Master, err)
		}
		result, err = ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			return fmt.Errorf("failed to set up IPAM plugin type %q from the device %q: %v", n.IPAM.Type, n.Master, err)
		}
		result.DNS = n.DNS

		// WIP save to file

		ipamResult, _ := json.Marshal(result)
		s := []string{args.ContainerID, n.DPDKConf.Ifname, "ipam"}
		filename := strings.Join(s, "-")
		if err := os.MkdirAll(n.CNIDir, 0700); err != nil {
			return fmt.Errorf("failed to create the sriov data directory(%q): %v", n.CNIDir, err)
		}
		path := filepath.Join(n.CNIDir, filename)

		err := ioutil.WriteFile(path, ipamResult, 0600)
		if err != nil {
			return fmt.Errorf("failed to write container data in the path(%q): %v", path, err)
		}
		return result.Print()
	}

	// run the IPAM plugin and get back the config to apply
	if !n.DPDKMode {
		result, err = ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			return fmt.Errorf("failed to set up IPAM plugin type %q from the device %q: %v", n.IPAM.Type, n.Master, err)
		}

		if result.IP4 == nil {
			return errors.New("IPAM plugin returned missing IPv4 config")
		}

		err = netns.Do(func(_ ns.NetNS) error {
			return ipam.ConfigureIface(args.IfName, result)
		})
		if err != nil {
			return err
		}
		result.DNS = n.DNS
	}

	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	n, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}

	if n.IF0NAME != "" {
		args.IfName = n.IF0NAME
	}

	// skip the IPAM release for L2 mode
	// TODO: what about DPDKMode?
	if !n.L2Mode && n.IPAM.Type != "" {
		err = ipam.ExecDel(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	if args.Netns == "" {
		return nil
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		// according to:
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		// if provided path does not exist (e.x. when node was restarted)
		// plugin should silently return with success after releasing
		// IPAM resources
		_, ok := err.(ns.NSPathNotExistErr)
		if ok {
			return nil
		}

		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	if err = releaseVF(n, args.IfName, args.ContainerID, netns); err != nil {
		return err
	}

	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.Legacy)
}
