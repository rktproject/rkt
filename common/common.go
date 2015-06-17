// Copyright 2014 The rkt Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package common defines values shared by different parts
// of rkt (e.g. stage0 and stage1)
package common

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/aci"
)

const (
	stage1Dir = "/stage1"
	stage2Dir = "/opt/stage2"

	EnvLockFd               = "RKT_LOCK_FD"
	Stage1IDFilename        = "stage1ID"
	OverlayPreparedFilename = "overlay-prepared"

	MetadataServicePort    = 2375
	MetadataServiceRegSock = "/run/rkt/metadata-svc.sock"

	DefaultLocalConfigDir  = "/etc/rkt"
	DefaultSystemConfigDir = "/usr/lib/rkt"
)

// Stage1ImagePath returns the path where the stage1 app image (unpacked ACI) is rooted,
// (i.e. where its contents are extracted during stage0).
func Stage1ImagePath(root string) string {
	return filepath.Join(root, stage1Dir)
}

// Stage1RootfsPath returns the path to the stage1 rootfs
func Stage1RootfsPath(root string) string {
	return filepath.Join(Stage1ImagePath(root), aci.RootfsDir)
}

// Stage1ManifestPath returns the path to the stage1's manifest file inside the expanded ACI.
func Stage1ManifestPath(root string) string {
	return filepath.Join(Stage1ImagePath(root), aci.ManifestFile)
}

// PodManifestPath returns the path in root to the Pod Manifest
func PodManifestPath(root string) string {
	return filepath.Join(root, "pod")
}

// AppsPath returns the path where the apps within a pod live.
func AppsPath(root string) string {
	return filepath.Join(Stage1RootfsPath(root), stage2Dir)
}

// AppPath returns the path where an app is rooted (i.e.
// where its ACI is extracted during stage0), based on the position of the app
// in the pod manifest.
func AppPath(root string, index int) string {
	return filepath.Join(AppsPath(root), strconv.Itoa(index))
}

// AppRootfsPath returns the path to an app's rootfs.
// index is the position of the app in the pod manifest.
func AppRootfsPath(root string, index int) string {
	return filepath.Join(AppPath(root, index), aci.RootfsDir)
}

// RelAppPath returns the path of an app relative to the stage1 chroot,
// based on the position of the app in the pod manfiest.
func RelAppPath(index int) string {
	return filepath.Join(stage2Dir, strconv.Itoa(index))
}

// RelAppRootfsPath returns the path of an app's rootfs relative to the stage1 chroot.
// index is the position of the app in the pod manifest.
func RelAppRootfsPath(index int) string {
	return filepath.Join(RelAppPath(index), aci.RootfsDir)
}

// ImageManifestPath returns the path to the app's manifest file inside a pod.
// index is the position of the app in the pod manifest.
func ImageManifestPath(root string, index int) string {
	return filepath.Join(AppPath(root, index), aci.ManifestFile)
}

// MetadataServicePublicURL returns the public URL used to host the metadata service
func MetadataServicePublicURL(ip net.IP) string {
	return fmt.Sprintf("http://%v:%v", ip, MetadataServicePort)
}

func GetRktLockFD() (int, error) {
	if v := os.Getenv(EnvLockFd); v != "" {
		fd, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return -1, err
		}
		return int(fd), nil
	}
	return -1, fmt.Errorf("%v env var is not set", EnvLockFd)
}

// SupportsOverlay returns whether the system supports overlay filesystem
func SupportsOverlay() bool {
	exec.Command("modprobe", "overlay").Run()

	f, err := os.Open("/proc/filesystems")
	if err != nil {
		fmt.Println("error opening /proc/filesystems")
		return false
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		if s.Text() == "nodev\toverlay" {
			return true
		}
	}
	return false
}

// PrivateNetList implements the flag.Value interface to allow specification
// of -private-net with and without values
type PrivateNetList struct {
	mapping map[string]bool
}

func (i *PrivateNetList) IsBoolFlag() bool { return true }

func (l *PrivateNetList) String() string {
	return strings.Join(l.Strings(), ",")
}

func (l *PrivateNetList) Set(value string) error {
	if l.mapping == nil {
		l.mapping = make(map[string]bool)
	}
	for _, s := range strings.Split(value, ",") {
		l.mapping[s] = true
	}
	return nil
}

func (l *PrivateNetList) Type() string {
	return "privateNetList"
}

func (l *PrivateNetList) Strings() []string {
	var list []string
	for k, _ := range l.mapping {
		list = append(list, k)
	}
	return list
}

func (l *PrivateNetList) Any() bool {
	return len(l.mapping) > 0
}

func (l *PrivateNetList) All() bool {
	return l.mapping["true"]
}

func (l *PrivateNetList) Specific(net string) bool {
	return l.mapping[net]
}
