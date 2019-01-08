// Copyright 2016 CNI authors
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

package libcni_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	noop_debug "github.com/containernetworking/cni/plugins/test/noop/debug"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type pluginInfo struct {
	debugFilePath string
	debug         *noop_debug.Debug
	config        string
	stdinData     []byte
}

type portMapping struct {
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

func stringInList(s string, list []string) bool {
	for _, item := range list {
		if s == item {
			return true
		}
	}
	return false
}

func newPluginInfo(configValue, prevResult string, injectDebugFilePath bool, result string, runtimeConfig map[string]interface{}, capabilities []string) pluginInfo {
	debugFile, err := ioutil.TempFile("", "cni_debug")
	Expect(err).NotTo(HaveOccurred())
	Expect(debugFile.Close()).To(Succeed())
	debugFilePath := debugFile.Name()

	debug := &noop_debug.Debug{
		ReportResult: result,
	}
	Expect(debug.WriteDebug(debugFilePath)).To(Succeed())

	// config is what would be in the plugin's on-disk configuration
	// without runtime injected keys
	config := fmt.Sprintf(`{"type": "noop", "some-key": "%s"`, configValue)
	if prevResult != "" {
		config += fmt.Sprintf(`, "prevResult": %s`, prevResult)
	}
	if injectDebugFilePath {
		config += fmt.Sprintf(`, "debugFile": %q`, debugFilePath)
	}
	if len(capabilities) > 0 {
		config += `, "capabilities": {`
		for i, c := range capabilities {
			if i > 0 {
				config += ", "
			}
			config += fmt.Sprintf(`"%s": true`, c)
		}
		config += "}"
	}
	config += "}"

	// stdinData is what the runtime should pass to the plugin's stdin,
	// including injected keys like 'name', 'cniVersion', and 'runtimeConfig'
	newConfig := make(map[string]interface{})
	err = json.Unmarshal([]byte(config), &newConfig)
	Expect(err).NotTo(HaveOccurred())
	newConfig["name"] = "some-list"
	newConfig["cniVersion"] = current.ImplementedSpecVersion

	// Only include standard runtime config and capability args that this plugin advertises
	newRuntimeConfig := make(map[string]interface{})
	for key, value := range runtimeConfig {
		if stringInList(key, capabilities) {
			newRuntimeConfig[key] = value
		}
	}
	if len(newRuntimeConfig) > 0 {
		newConfig["runtimeConfig"] = newRuntimeConfig
	}

	stdinData, err := json.Marshal(newConfig)
	Expect(err).NotTo(HaveOccurred())

	return pluginInfo{
		debugFilePath: debugFilePath,
		debug:         debug,
		config:        config,
		stdinData:     stdinData,
	}
}

func resultCacheFilePath(cacheDirPath, netName, containerID string) string {
	return filepath.Join(cacheDirPath, "results", netName+"-"+containerID)
}

var _ = Describe("Invoking plugins", func() {
	var cacheDirPath string

	BeforeEach(func() {
		var err error
		cacheDirPath, err = ioutil.TempDir("", "cni_cachedir")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(os.RemoveAll(cacheDirPath)).To(Succeed())
	})

	Describe("Capabilities", func() {
		var (
			debugFilePath string
			debug         *noop_debug.Debug
			pluginConfig  []byte
			cniConfig     *libcni.CNIConfig
			runtimeConfig *libcni.RuntimeConf
			netConfig     *libcni.NetworkConfig
		)

		BeforeEach(func() {
			debugFile, err := ioutil.TempFile("", "cni_debug")
			Expect(err).NotTo(HaveOccurred())
			Expect(debugFile.Close()).To(Succeed())
			debugFilePath = debugFile.Name()

			debug = &noop_debug.Debug{}
			Expect(debug.WriteDebug(debugFilePath)).To(Succeed())

			pluginConfig = []byte(fmt.Sprintf(`{
				"type": "noop",
				"name": "apitest",
				"cniVersion": "%s",
				"capabilities": {
					"portMappings": true,
					"somethingElse": true,
					"noCapability": false
				}
			}`, current.ImplementedSpecVersion))
			netConfig, err = libcni.ConfFromBytes(pluginConfig)
			Expect(err).NotTo(HaveOccurred())

			cniConfig = libcni.NewCNIConfig([]string{filepath.Dir(pluginPaths["noop"])}, nil)

			runtimeConfig = &libcni.RuntimeConf{
				ContainerID: "some-container-id",
				NetNS:       "/some/netns/path",
				IfName:      "some-eth0",
				Args:        [][2]string{{"DEBUG", debugFilePath}},
				CapabilityArgs: map[string]interface{}{
					"portMappings": []portMapping{
						{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
					},
					"somethingElse": []string{"foobar", "baz"},
					"noCapability":  true,
					"notAdded":      []bool{true, false},
				},
				CacheDir: cacheDirPath,
			}
		})

		AfterEach(func() {
			Expect(os.RemoveAll(debugFilePath)).To(Succeed())
		})

		It("adds correct runtime config for capabilities to stdin", func() {
			_, err := cniConfig.AddNetwork(netConfig, runtimeConfig)
			Expect(err).NotTo(HaveOccurred())

			debug, err = noop_debug.ReadDebug(debugFilePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(debug.Command).To(Equal("ADD"))

			conf := make(map[string]interface{})
			err = json.Unmarshal(debug.CmdArgs.StdinData, &conf)
			Expect(err).NotTo(HaveOccurred())

			// We expect runtimeConfig keys only for portMappings and somethingElse
			rawRc := conf["runtimeConfig"]
			rc, ok := rawRc.(map[string]interface{})
			Expect(ok).To(Equal(true))
			expectedKeys := []string{"portMappings", "somethingElse"}
			Expect(len(rc)).To(Equal(len(expectedKeys)))
			for _, key := range expectedKeys {
				_, ok := rc[key]
				Expect(ok).To(Equal(true))
			}
		})

		It("adds no runtimeConfig when the plugin advertises no used capabilities", func() {
			// Replace CapabilityArgs with ones we know the plugin
			// doesn't support
			runtimeConfig.CapabilityArgs = map[string]interface{}{
				"portMappings22": []portMapping{
					{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
				},
				"somethingElse22": []string{"foobar", "baz"},
			}

			_, err := cniConfig.AddNetwork(netConfig, runtimeConfig)
			Expect(err).NotTo(HaveOccurred())

			debug, err = noop_debug.ReadDebug(debugFilePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(debug.Command).To(Equal("ADD"))

			conf := make(map[string]interface{})
			err = json.Unmarshal(debug.CmdArgs.StdinData, &conf)
			Expect(err).NotTo(HaveOccurred())

			// No intersection of plugin capabilities and CapabilityArgs,
			// so plugin should not receive a "runtimeConfig" key
			_, ok := conf["runtimeConfig"]
			Expect(ok).Should(BeFalse())
		})
	})

	Describe("Invoking a single plugin", func() {
		var (
			debugFilePath string
			debug         *noop_debug.Debug
			cniBinPath    string
			pluginConfig  string
			cniConfig     *libcni.CNIConfig
			netConfig     *libcni.NetworkConfig
			runtimeConfig *libcni.RuntimeConf

			expectedCmdArgs skel.CmdArgs
		)

		BeforeEach(func() {
			debugFile, err := ioutil.TempFile("", "cni_debug")
			Expect(err).NotTo(HaveOccurred())
			Expect(debugFile.Close()).To(Succeed())
			debugFilePath = debugFile.Name()

			debug = &noop_debug.Debug{
				ReportResult: `{
					"cniVersion": "0.4.0",
					"ips": [{"version": "4", "address": "10.1.2.3/24"}],
					"dns": {}
				}`,
			}
			Expect(debug.WriteDebug(debugFilePath)).To(Succeed())

			portMappings := []portMapping{
				{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
			}

			cniBinPath = filepath.Dir(pluginPaths["noop"])
			pluginConfig = fmt.Sprintf(`{
				"type": "noop",
				"name": "apitest",
				"some-key": "some-value",
				"cniVersion": "%s",
				"capabilities": { "portMappings": true }
			}`, current.ImplementedSpecVersion)
			cniConfig = libcni.NewCNIConfig([]string{cniBinPath}, nil)
			netConfig, err = libcni.ConfFromBytes([]byte(pluginConfig))
			Expect(err).NotTo(HaveOccurred())
			runtimeConfig = &libcni.RuntimeConf{
				ContainerID: "some-container-id",
				NetNS:       "/some/netns/path",
				IfName:      "some-eth0",
				CacheDir:    cacheDirPath,
				Args:        [][2]string{{"DEBUG", debugFilePath}},
				CapabilityArgs: map[string]interface{}{
					"portMappings": portMappings,
				},
			}

			// inject runtime args into the expected plugin config
			conf := make(map[string]interface{})
			err = json.Unmarshal([]byte(pluginConfig), &conf)
			Expect(err).NotTo(HaveOccurred())
			conf["runtimeConfig"] = map[string]interface{}{
				"portMappings": portMappings,
			}
			newBytes, err := json.Marshal(conf)
			Expect(err).NotTo(HaveOccurred())

			expectedCmdArgs = skel.CmdArgs{
				ContainerID: "some-container-id",
				Netns:       "/some/netns/path",
				IfName:      "some-eth0",
				Args:        "DEBUG=" + debugFilePath,
				Path:        cniBinPath,
				StdinData:   newBytes,
			}
		})

		AfterEach(func() {
			Expect(os.RemoveAll(debugFilePath)).To(Succeed())
		})

		Describe("AddNetwork", func() {
			It("executes the plugin with command ADD", func() {
				r, err := cniConfig.AddNetwork(netConfig, runtimeConfig)
				Expect(err).NotTo(HaveOccurred())

				result, err := current.GetResult(r)
				Expect(err).NotTo(HaveOccurred())

				Expect(result).To(Equal(&current.Result{
					CNIVersion: current.ImplementedSpecVersion,
					IPs: []*current.IPConfig{
						{
							Version: "4",
							Address: net.IPNet{
								IP:   net.ParseIP("10.1.2.3"),
								Mask: net.IPv4Mask(255, 255, 255, 0),
							},
						},
					},
				}))

				debug, err := noop_debug.ReadDebug(debugFilePath)
				Expect(err).NotTo(HaveOccurred())
				Expect(debug.Command).To(Equal("ADD"))
				Expect(debug.CmdArgs).To(Equal(expectedCmdArgs))
				Expect(string(debug.CmdArgs.StdinData)).To(ContainSubstring("\"portMappings\":"))

				// Ensure the cached result matches the returned one
				cacheFile := resultCacheFilePath(cacheDirPath, netConfig.Network.Name, runtimeConfig.ContainerID)
				_, err = os.Stat(cacheFile)
				Expect(err).NotTo(HaveOccurred())
				cachedData, err := ioutil.ReadFile(cacheFile)
				Expect(err).NotTo(HaveOccurred())
				returnedData, err := json.Marshal(result)
				Expect(err).NotTo(HaveOccurred())
				Expect(cachedData).To(MatchJSON(returnedData))
			})

			Context("when finding the plugin fails", func() {
				BeforeEach(func() {
					netConfig.Network.Type = "does-not-exist"
				})

				It("returns the error", func() {
					_, err := cniConfig.AddNetwork(netConfig, runtimeConfig)
					Expect(err).To(MatchError(ContainSubstring(`failed to find plugin "does-not-exist"`)))
				})
			})

			Context("when the plugin errors", func() {
				BeforeEach(func() {
					debug.ReportError = "plugin error: banana"
					Expect(debug.WriteDebug(debugFilePath)).To(Succeed())
				})
				It("unmarshals and returns the error", func() {
					result, err := cniConfig.AddNetwork(netConfig, runtimeConfig)
					Expect(result).To(BeNil())
					Expect(err).To(MatchError("plugin error: banana"))
				})
			})

			Context("when the result cache directory cannot be accessed", func() {
				It("returns an error", func() {
					// Make the results directory inaccessble by making it a
					// file instead of a directory
					tmpPath := filepath.Join(cacheDirPath, "results")
					err := ioutil.WriteFile(tmpPath, []byte("afdsasdfasdf"), 0600)
					Expect(err).NotTo(HaveOccurred())

					result, err := cniConfig.AddNetwork(netConfig, runtimeConfig)
					Expect(result).To(BeNil())
					Expect(err).To(HaveOccurred())
				})
			})
		})

		Describe("GetNetwork", func() {
			It("executes the plugin with command GET", func() {
				cacheFile := resultCacheFilePath(cacheDirPath, netConfig.Network.Name, runtimeConfig.ContainerID)
				err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
				Expect(err).NotTo(HaveOccurred())
				cachedJson := `{
					"cniVersion": "0.4.0",
					"ips": [{"version": "4", "address": "10.1.2.3/24"}],
					"dns": {}
				}`
				err = ioutil.WriteFile(cacheFile, []byte(cachedJson), 0600)
				Expect(err).NotTo(HaveOccurred())

				r, err := cniConfig.GetNetwork(netConfig, runtimeConfig)
				Expect(err).NotTo(HaveOccurred())

				result, err := current.GetResult(r)
				Expect(err).NotTo(HaveOccurred())

				Expect(result).To(Equal(&current.Result{
					CNIVersion: current.ImplementedSpecVersion,
					IPs: []*current.IPConfig{
						{
							Version: "4",
							Address: net.IPNet{
								IP:   net.ParseIP("10.1.2.3"),
								Mask: net.IPv4Mask(255, 255, 255, 0),
							},
						},
					},
				}))

				debug, err := noop_debug.ReadDebug(debugFilePath)
				Expect(err).NotTo(HaveOccurred())
				Expect(debug.Command).To(Equal("GET"))
				Expect(string(debug.CmdArgs.StdinData)).To(ContainSubstring("\"portMappings\":"))

				// Explicitly match stdin data as json after
				// inserting the expected prevResult
				var data, data2 map[string]interface{}
				err = json.Unmarshal(expectedCmdArgs.StdinData, &data)
				Expect(err).NotTo(HaveOccurred())
				err = json.Unmarshal([]byte(cachedJson), &data2)
				Expect(err).NotTo(HaveOccurred())
				data["prevResult"] = data2
				expectedStdinJson, err := json.Marshal(data)
				Expect(err).NotTo(HaveOccurred())
				Expect(debug.CmdArgs.StdinData).To(MatchJSON(expectedStdinJson))

				debug.CmdArgs.StdinData = nil
				expectedCmdArgs.StdinData = nil
				Expect(debug.CmdArgs).To(Equal(expectedCmdArgs))
			})

			Context("when finding the plugin fails", func() {
				BeforeEach(func() {
					netConfig.Network.Type = "does-not-exist"
				})

				It("returns the error", func() {
					_, err := cniConfig.GetNetwork(netConfig, runtimeConfig)
					Expect(err).To(MatchError(ContainSubstring(`failed to find plugin "does-not-exist"`)))
				})
			})

			Context("when the plugin errors", func() {
				BeforeEach(func() {
					debug.ReportError = "plugin error: banana"
					Expect(debug.WriteDebug(debugFilePath)).To(Succeed())
				})
				It("unmarshals and returns the error", func() {
					result, err := cniConfig.GetNetwork(netConfig, runtimeConfig)
					Expect(result).To(BeNil())
					Expect(err).To(MatchError("plugin error: banana"))
				})
			})

			Context("when GET is called with a configuration version", func() {
				var cacheFile string

				BeforeEach(func() {
					cacheFile = resultCacheFilePath(cacheDirPath, netConfig.Network.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
				})

				Context("less than 0.4.0", func() {
					It("fails as GET is not supported before 0.4.0", func() {
						// Generate plugin config with older version
						pluginConfig = `{
							"type": "noop",
							"name": "apitest",
							"cniVersion": "0.3.1"
						}`
						var err error
						netConfig, err = libcni.ConfFromBytes([]byte(pluginConfig))
						Expect(err).NotTo(HaveOccurred())
						_, err = cniConfig.GetNetwork(netConfig, runtimeConfig)
						Expect(err).To(MatchError("configuration version \"0.3.1\" does not support the GET command"))

						debug, err := noop_debug.ReadDebug(debugFilePath)
						Expect(err).NotTo(HaveOccurred())
						Expect(string(debug.CmdArgs.StdinData)).NotTo(ContainSubstring("\"prevResult\":"))
					})
				})

				Context("equal to 0.4.0", func() {
					It("passes a prevResult to the plugin", func() {
						err := ioutil.WriteFile(cacheFile, []byte(`{
							"cniVersion": "0.4.0",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`), 0600)
						Expect(err).NotTo(HaveOccurred())

						_, err = cniConfig.GetNetwork(netConfig, runtimeConfig)
						Expect(err).NotTo(HaveOccurred())
						debug, err := noop_debug.ReadDebug(debugFilePath)
						Expect(err).NotTo(HaveOccurred())
						Expect(string(debug.CmdArgs.StdinData)).To(ContainSubstring("\"prevResult\":"))
					})
				})
			})

			Context("when the cached result", func() {
				var cacheFile string

				BeforeEach(func() {
					cacheFile = resultCacheFilePath(cacheDirPath, netConfig.Network.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
				})

				Context("is invalid JSON", func() {
					It("returns an error", func() {
						err := ioutil.WriteFile(cacheFile, []byte("adfadsfasdfasfdsafaf"), 0600)
						Expect(err).NotTo(HaveOccurred())

						result, err := cniConfig.GetNetwork(netConfig, runtimeConfig)
						Expect(result).To(BeNil())
						Expect(err).To(MatchError("failed to get network 'apitest' cached result: decoding version from network config: invalid character 'a' looking for beginning of value"))
					})
				})

				Context("version doesn't match the config version", func() {
					It("succeeds when the cached result can be converted", func() {
						err := ioutil.WriteFile(cacheFile, []byte(`{
							"cniVersion": "0.3.1",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`), 0600)
						Expect(err).NotTo(HaveOccurred())

						_, err = cniConfig.GetNetwork(netConfig, runtimeConfig)
						Expect(err).NotTo(HaveOccurred())
					})

					It("returns an error when the cached result cannot be converted", func() {
						err := ioutil.WriteFile(cacheFile, []byte(`{
							"cniVersion": "0.4567.0",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`), 0600)
						Expect(err).NotTo(HaveOccurred())

						result, err := cniConfig.GetNetwork(netConfig, runtimeConfig)
						Expect(result).To(BeNil())
						Expect(err).To(MatchError("failed to get network 'apitest' cached result: unsupported CNI result version \"0.4567.0\""))
					})
				})
			})
		})

		Describe("DelNetwork", func() {
			It("executes the plugin with command DEL", func() {
				cacheFile := resultCacheFilePath(cacheDirPath, netConfig.Network.Name, runtimeConfig.ContainerID)
				err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
				Expect(err).NotTo(HaveOccurred())
				cachedJson := `{
					"cniVersion": "0.4.0",
					"ips": [{"version": "4", "address": "10.1.2.3/24"}],
					"dns": {}
				}`
				err = ioutil.WriteFile(cacheFile, []byte(cachedJson), 0600)
				Expect(err).NotTo(HaveOccurred())

				err = cniConfig.DelNetwork(netConfig, runtimeConfig)
				Expect(err).NotTo(HaveOccurred())

				debug, err := noop_debug.ReadDebug(debugFilePath)
				Expect(err).NotTo(HaveOccurred())
				Expect(debug.Command).To(Equal("DEL"))
				Expect(string(debug.CmdArgs.StdinData)).To(ContainSubstring("\"portMappings\":"))

				// Explicitly match stdin data as json after
				// inserting the expected prevResult
				var data, data2 map[string]interface{}
				err = json.Unmarshal(expectedCmdArgs.StdinData, &data)
				Expect(err).NotTo(HaveOccurred())
				err = json.Unmarshal([]byte(cachedJson), &data2)
				Expect(err).NotTo(HaveOccurred())
				data["prevResult"] = data2
				expectedStdinJson, err := json.Marshal(data)
				Expect(err).NotTo(HaveOccurred())
				Expect(debug.CmdArgs.StdinData).To(MatchJSON(expectedStdinJson))

				debug.CmdArgs.StdinData = nil
				expectedCmdArgs.StdinData = nil
				Expect(debug.CmdArgs).To(Equal(expectedCmdArgs))
			})

			Context("when finding the plugin fails", func() {
				BeforeEach(func() {
					netConfig.Network.Type = "does-not-exist"
				})

				It("returns the error", func() {
					err := cniConfig.DelNetwork(netConfig, runtimeConfig)
					Expect(err).To(MatchError(ContainSubstring(`failed to find plugin "does-not-exist"`)))
				})
			})

			Context("when the plugin errors", func() {
				BeforeEach(func() {
					debug.ReportError = "plugin error: banana"
					Expect(debug.WriteDebug(debugFilePath)).To(Succeed())
				})
				It("unmarshals and returns the error", func() {
					err := cniConfig.DelNetwork(netConfig, runtimeConfig)
					Expect(err).To(MatchError("plugin error: banana"))
				})
			})

			Context("when DEL is called twice", func() {
				var cacheFile string

				BeforeEach(func() {
					cacheFile = resultCacheFilePath(cacheDirPath, netConfig.Network.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
				})

				It("deletes the cached result after the first DEL", func() {
					err := ioutil.WriteFile(cacheFile, []byte(`{
						"cniVersion": "0.4.0",
						"ips": [{"version": "4", "address": "10.1.2.3/24"}],
						"dns": {}
					}`), 0600)
					Expect(err).NotTo(HaveOccurred())

					err = cniConfig.DelNetwork(netConfig, runtimeConfig)
					Expect(err).NotTo(HaveOccurred())
					_, err = ioutil.ReadFile(cacheFile)
					Expect(err).To(HaveOccurred())

					err = cniConfig.DelNetwork(netConfig, runtimeConfig)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			Context("when DEL is called with a configuration version", func() {
				var cacheFile string

				BeforeEach(func() {
					cacheFile = resultCacheFilePath(cacheDirPath, netConfig.Network.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
				})

				Context("less than 0.4.0", func() {
					It("does not pass a prevResult to the plugin", func() {
						err := ioutil.WriteFile(cacheFile, []byte(`{
							"cniVersion": "0.3.1",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`), 0600)
						Expect(err).NotTo(HaveOccurred())

						// Generate plugin config with older version
						pluginConfig = `{
							"type": "noop",
							"name": "apitest",
							"cniVersion": "0.3.1"
						}`
						netConfig, err = libcni.ConfFromBytes([]byte(pluginConfig))
						Expect(err).NotTo(HaveOccurred())
						err = cniConfig.DelNetwork(netConfig, runtimeConfig)
						Expect(err).NotTo(HaveOccurred())

						debug, err := noop_debug.ReadDebug(debugFilePath)
						Expect(err).NotTo(HaveOccurred())
						Expect(debug.Command).To(Equal("DEL"))
						Expect(string(debug.CmdArgs.StdinData)).NotTo(ContainSubstring("\"prevResult\":"))
					})
				})

				Context("equal to 0.4.0", func() {
					It("passes a prevResult to the plugin", func() {
						err := ioutil.WriteFile(cacheFile, []byte(`{
							"cniVersion": "0.4.0",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`), 0600)
						Expect(err).NotTo(HaveOccurred())

						err = cniConfig.DelNetwork(netConfig, runtimeConfig)
						Expect(err).NotTo(HaveOccurred())

						debug, err := noop_debug.ReadDebug(debugFilePath)
						Expect(err).NotTo(HaveOccurred())
						Expect(debug.Command).To(Equal("DEL"))
						Expect(string(debug.CmdArgs.StdinData)).To(ContainSubstring("\"prevResult\":"))
					})
				})
			})

			Context("when the cached result", func() {
				var cacheFile string

				BeforeEach(func() {
					cacheFile = resultCacheFilePath(cacheDirPath, netConfig.Network.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
				})

				Context("is invalid JSON", func() {
					It("returns an error", func() {
						err := ioutil.WriteFile(cacheFile, []byte("adfadsfasdfasfdsafaf"), 0600)
						Expect(err).NotTo(HaveOccurred())

						err = cniConfig.DelNetwork(netConfig, runtimeConfig)
						Expect(err).To(MatchError("failed to get network 'apitest' cached result: decoding version from network config: invalid character 'a' looking for beginning of value"))
					})
				})

				Context("version doesn't match the config version", func() {
					It("succeeds when the cached result can be converted", func() {
						err := ioutil.WriteFile(cacheFile, []byte(`{
							"cniVersion": "0.3.1",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`), 0600)
						Expect(err).NotTo(HaveOccurred())

						err = cniConfig.DelNetwork(netConfig, runtimeConfig)
						Expect(err).NotTo(HaveOccurred())
					})

					It("returns an error when the cached result cannot be converted", func() {
						err := ioutil.WriteFile(cacheFile, []byte(`{
							"cniVersion": "0.4567.0",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`), 0600)
						Expect(err).NotTo(HaveOccurred())

						err = cniConfig.DelNetwork(netConfig, runtimeConfig)
						Expect(err).To(MatchError("failed to get network 'apitest' cached result: unsupported CNI result version \"0.4567.0\""))
					})
				})
			})
		})

		Describe("GetVersionInfo", func() {
			It("executes the plugin with the command VERSION", func() {
				versionInfo, err := cniConfig.GetVersionInfo("noop")
				Expect(err).NotTo(HaveOccurred())

				Expect(versionInfo).NotTo(BeNil())
				Expect(versionInfo.SupportedVersions()).To(Equal([]string{
					"0.-42.0", "0.1.0", "0.2.0", "0.3.0", "0.3.1", "0.4.0",
				}))
			})

			Context("when finding the plugin fails", func() {
				It("returns the error", func() {
					_, err := cniConfig.GetVersionInfo("does-not-exist")
					Expect(err).To(MatchError(ContainSubstring(`failed to find plugin "does-not-exist"`)))
				})
			})
		})
	})

	Describe("Invoking a plugin list", func() {
		var (
			plugins       []pluginInfo
			cniBinPath    string
			cniConfig     *libcni.CNIConfig
			netConfigList *libcni.NetworkConfigList
			runtimeConfig *libcni.RuntimeConf

			expectedCmdArgs skel.CmdArgs
		)

		BeforeEach(func() {
			var err error

			capabilityArgs := map[string]interface{}{
				"portMappings": []portMapping{
					{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
				},
				"otherCapability": 33,
			}

			cniBinPath = filepath.Dir(pluginPaths["noop"])
			cniConfig = libcni.NewCNIConfig([]string{cniBinPath}, nil)
			runtimeConfig = &libcni.RuntimeConf{
				ContainerID:    "some-container-id",
				NetNS:          "/some/netns/path",
				IfName:         "some-eth0",
				Args:           [][2]string{{"FOO", "BAR"}},
				CapabilityArgs: capabilityArgs,
				CacheDir:       cacheDirPath,
			}

			expectedCmdArgs = skel.CmdArgs{
				ContainerID: runtimeConfig.ContainerID,
				Netns:       runtimeConfig.NetNS,
				IfName:      runtimeConfig.IfName,
				Args:        "FOO=BAR",
				Path:        cniBinPath,
			}

			rc := map[string]interface{}{
				"containerId": runtimeConfig.ContainerID,
				"netNs":       runtimeConfig.NetNS,
				"ifName":      runtimeConfig.IfName,
				"args": map[string]string{
					"FOO": "BAR",
				},
				"portMappings":    capabilityArgs["portMappings"],
				"otherCapability": capabilityArgs["otherCapability"],
			}

			ipResult := fmt.Sprintf(`{"cniVersion": "%s", "dns":{},"ips":[{"version": "4", "address": "10.1.2.3/24"}]}`, current.ImplementedSpecVersion)
			plugins = make([]pluginInfo, 3, 3)
			plugins[0] = newPluginInfo("some-value", "", true, ipResult, rc, []string{"portMappings", "otherCapability"})
			plugins[1] = newPluginInfo("some-other-value", ipResult, true, "PASSTHROUGH", rc, []string{"otherCapability"})
			plugins[2] = newPluginInfo("yet-another-value", ipResult, true, "INJECT-DNS", rc, []string{})

			configList := []byte(fmt.Sprintf(`{
  "name": "some-list",
  "cniVersion": "%s",
  "plugins": [
    %s,
    %s,
    %s
  ]
}`, current.ImplementedSpecVersion, plugins[0].config, plugins[1].config, plugins[2].config))

			netConfigList, err = libcni.ConfListFromBytes(configList)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			for _, p := range plugins {
				Expect(os.RemoveAll(p.debugFilePath)).To(Succeed())
			}
		})

		Describe("AddNetworkList", func() {
			It("executes all plugins with command ADD and returns an intermediate result", func() {
				r, err := cniConfig.AddNetworkList(netConfigList, runtimeConfig)
				Expect(err).NotTo(HaveOccurred())

				result, err := current.GetResult(r)
				Expect(err).NotTo(HaveOccurred())

				Expect(result).To(Equal(&current.Result{
					CNIVersion: current.ImplementedSpecVersion,
					// IP4 added by first plugin
					IPs: []*current.IPConfig{
						{
							Version: "4",
							Address: net.IPNet{
								IP:   net.ParseIP("10.1.2.3"),
								Mask: net.IPv4Mask(255, 255, 255, 0),
							},
						},
					},
					// DNS injected by last plugin
					DNS: types.DNS{
						Nameservers: []string{"1.2.3.4"},
					},
				}))

				for i := 0; i < len(plugins); i++ {
					debug, err := noop_debug.ReadDebug(plugins[i].debugFilePath)
					Expect(err).NotTo(HaveOccurred())
					Expect(debug.Command).To(Equal("ADD"))

					// Must explicitly match JSON due to dict element ordering
					Expect(debug.CmdArgs.StdinData).To(MatchJSON(plugins[i].stdinData))
					debug.CmdArgs.StdinData = nil
					Expect(debug.CmdArgs).To(Equal(expectedCmdArgs))
				}
			})

			It("writes the correct cached result", func() {
				r, err := cniConfig.AddNetworkList(netConfigList, runtimeConfig)
				Expect(err).NotTo(HaveOccurred())

				result, err := current.GetResult(r)
				Expect(err).NotTo(HaveOccurred())

				// Ensure the cached result matches the returned one
				cacheFile := resultCacheFilePath(cacheDirPath, netConfigList.Name, runtimeConfig.ContainerID)
				_, err = os.Stat(cacheFile)
				Expect(err).NotTo(HaveOccurred())
				cachedData, err := ioutil.ReadFile(cacheFile)
				Expect(err).NotTo(HaveOccurred())
				returnedData, err := json.Marshal(result)
				Expect(err).NotTo(HaveOccurred())
				Expect(cachedData).To(MatchJSON(returnedData))
			})

			Context("when finding the plugin fails", func() {
				BeforeEach(func() {
					netConfigList.Plugins[1].Network.Type = "does-not-exist"
				})

				It("returns the error", func() {
					_, err := cniConfig.AddNetworkList(netConfigList, runtimeConfig)
					Expect(err).To(MatchError(ContainSubstring(`failed to find plugin "does-not-exist"`)))
				})
			})

			Context("when the second plugin errors", func() {
				BeforeEach(func() {
					plugins[1].debug.ReportError = "plugin error: banana"
					Expect(plugins[1].debug.WriteDebug(plugins[1].debugFilePath)).To(Succeed())
				})
				It("unmarshals and returns the error", func() {
					result, err := cniConfig.AddNetworkList(netConfigList, runtimeConfig)
					Expect(result).To(BeNil())
					Expect(err).To(MatchError("plugin error: banana"))
				})
			})

			Context("when the result cache directory cannot be accessed", func() {
				It("returns an error", func() {
					// Make the results directory inaccessble by making it a
					// file instead of a directory
					tmpPath := filepath.Join(cacheDirPath, "results")
					err := ioutil.WriteFile(tmpPath, []byte("afdsasdfasdf"), 0600)
					Expect(err).NotTo(HaveOccurred())

					result, err := cniConfig.AddNetworkList(netConfigList, runtimeConfig)
					Expect(result).To(BeNil())
					Expect(err).To(HaveOccurred())
				})
			})
		})

		Describe("GetNetworkList", func() {
			It("executes all plugins with command GET and returns an intermediate result", func() {
				r, err := cniConfig.GetNetworkList(netConfigList, runtimeConfig)
				Expect(err).NotTo(HaveOccurred())

				result, err := current.GetResult(r)
				Expect(err).NotTo(HaveOccurred())

				Expect(result).To(Equal(&current.Result{
					CNIVersion: current.ImplementedSpecVersion,
					// IP4 added by first plugin
					IPs: []*current.IPConfig{
						{
							Version: "4",
							Address: net.IPNet{
								IP:   net.ParseIP("10.1.2.3"),
								Mask: net.IPv4Mask(255, 255, 255, 0),
							},
						},
					},
					// DNS injected by last plugin
					DNS: types.DNS{
						Nameservers: []string{"1.2.3.4"},
					},
				}))

				for i := 0; i < len(plugins); i++ {
					debug, err := noop_debug.ReadDebug(plugins[i].debugFilePath)
					Expect(err).NotTo(HaveOccurred())
					Expect(debug.Command).To(Equal("GET"))

					// Must explicitly match JSON due to dict element ordering
					Expect(debug.CmdArgs.StdinData).To(MatchJSON(plugins[i].stdinData))
					debug.CmdArgs.StdinData = nil
					Expect(debug.CmdArgs).To(Equal(expectedCmdArgs))
				}
			})

			Context("when the configuration version", func() {
				var cacheFile string

				BeforeEach(func() {
					cacheFile = resultCacheFilePath(cacheDirPath, netConfigList.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
				})

				Context("is 0.4.0", func() {
					It("passes a cached result to the first plugin", func() {
						cachedJson := `{
							"cniVersion": "0.4.0",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`
						err := ioutil.WriteFile(cacheFile, []byte(cachedJson), 0600)
						Expect(err).NotTo(HaveOccurred())

						_, err = cniConfig.GetNetworkList(netConfigList, runtimeConfig)
						Expect(err).NotTo(HaveOccurred())

						// Match the first plugin's stdin config to the cached result JSON
						debug, err := noop_debug.ReadDebug(plugins[0].debugFilePath)
						Expect(err).NotTo(HaveOccurred())

						var data map[string]interface{}
						err = json.Unmarshal(debug.CmdArgs.StdinData, &data)
						Expect(err).NotTo(HaveOccurred())
						stdinPrevResult, err := json.Marshal(data["prevResult"])
						Expect(err).NotTo(HaveOccurred())
						Expect(stdinPrevResult).To(MatchJSON(cachedJson))
					})
				})

				Context("is less than 0.4.0", func() {
					It("fails as GET is not supported before 0.4.0", func() {
						// Set an older CNI version
						confList := make(map[string]interface{})
						err := json.Unmarshal(netConfigList.Bytes, &confList)
						Expect(err).NotTo(HaveOccurred())
						confList["cniVersion"] = "0.3.1"
						newBytes, err := json.Marshal(confList)
						Expect(err).NotTo(HaveOccurred())

						netConfigList, err = libcni.ConfListFromBytes(newBytes)
						Expect(err).NotTo(HaveOccurred())
						_, err = cniConfig.GetNetworkList(netConfigList, runtimeConfig)
						Expect(err).To(MatchError("configuration version \"0.3.1\" does not support the GET command"))
					})
				})
			})

			Context("when finding the plugin fails", func() {
				BeforeEach(func() {
					netConfigList.Plugins[1].Network.Type = "does-not-exist"
				})

				It("returns the error", func() {
					_, err := cniConfig.GetNetworkList(netConfigList, runtimeConfig)
					Expect(err).To(MatchError(ContainSubstring(`failed to find plugin "does-not-exist"`)))
				})
			})

			Context("when the second plugin errors", func() {
				BeforeEach(func() {
					plugins[1].debug.ReportError = "plugin error: banana"
					Expect(plugins[1].debug.WriteDebug(plugins[1].debugFilePath)).To(Succeed())
				})
				It("unmarshals and returns the error", func() {
					result, err := cniConfig.GetNetworkList(netConfigList, runtimeConfig)
					Expect(result).To(BeNil())
					Expect(err).To(MatchError("plugin error: banana"))
				})
			})

			Context("when the cached result is invalid", func() {
				It("returns an error", func() {
					cacheFile := resultCacheFilePath(cacheDirPath, netConfigList.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
					err = ioutil.WriteFile(cacheFile, []byte("adfadsfasdfasfdsafaf"), 0600)
					Expect(err).NotTo(HaveOccurred())

					result, err := cniConfig.GetNetworkList(netConfigList, runtimeConfig)
					Expect(result).To(BeNil())
					Expect(err).To(MatchError("failed to get network 'some-list' cached result: decoding version from network config: invalid character 'a' looking for beginning of value"))
				})
			})
		})

		Describe("DelNetworkList", func() {
			It("executes all the plugins in reverse order with command DEL", func() {
				err := cniConfig.DelNetworkList(netConfigList, runtimeConfig)
				Expect(err).NotTo(HaveOccurred())

				for i := 0; i < len(plugins); i++ {
					debug, err := noop_debug.ReadDebug(plugins[i].debugFilePath)
					Expect(err).NotTo(HaveOccurred())
					Expect(debug.Command).To(Equal("DEL"))

					// Must explicitly match JSON due to dict element ordering
					Expect(debug.CmdArgs.StdinData).To(MatchJSON(plugins[i].stdinData))
					debug.CmdArgs.StdinData = nil
					Expect(debug.CmdArgs).To(Equal(expectedCmdArgs))
				}
			})

			Context("when the configuration version", func() {
				var cacheFile string

				BeforeEach(func() {
					cacheFile = resultCacheFilePath(cacheDirPath, netConfigList.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
				})

				Context("is 0.4.0", func() {
					It("passes a cached result to the first plugin", func() {
						cachedJson := `{
							"cniVersion": "0.4.0",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`
						err := ioutil.WriteFile(cacheFile, []byte(cachedJson), 0600)
						Expect(err).NotTo(HaveOccurred())

						err = cniConfig.DelNetworkList(netConfigList, runtimeConfig)
						Expect(err).NotTo(HaveOccurred())

						// Match the first plugin's stdin config to the cached result JSON
						debug, err := noop_debug.ReadDebug(plugins[0].debugFilePath)
						Expect(err).NotTo(HaveOccurred())

						var data map[string]interface{}
						err = json.Unmarshal(debug.CmdArgs.StdinData, &data)
						Expect(err).NotTo(HaveOccurred())
						stdinPrevResult, err := json.Marshal(data["prevResult"])
						Expect(err).NotTo(HaveOccurred())
						Expect(stdinPrevResult).To(MatchJSON(cachedJson))
					})
				})

				Context("is less than 0.4.0", func() {
					It("does not pass a cached result to the first plugin", func() {
						err := ioutil.WriteFile(cacheFile, []byte(`{
							"cniVersion": "0.3.1",
							"ips": [{"version": "4", "address": "10.1.2.3/24"}],
							"dns": {}
						}`), 0600)
						Expect(err).NotTo(HaveOccurred())

						// Set an older CNI version
						confList := make(map[string]interface{})
						err = json.Unmarshal(netConfigList.Bytes, &confList)
						Expect(err).NotTo(HaveOccurred())
						confList["cniVersion"] = "0.3.1"
						newBytes, err := json.Marshal(confList)
						Expect(err).NotTo(HaveOccurred())

						netConfigList, err = libcni.ConfListFromBytes(newBytes)
						Expect(err).NotTo(HaveOccurred())
						err = cniConfig.DelNetworkList(netConfigList, runtimeConfig)
						Expect(err).NotTo(HaveOccurred())

						// Make sure first plugin does not receive a prevResult
						debug, err := noop_debug.ReadDebug(plugins[0].debugFilePath)
						Expect(err).NotTo(HaveOccurred())
						Expect(string(debug.CmdArgs.StdinData)).NotTo(ContainSubstring("\"prevResult\":"))
					})
				})
			})

			Context("when finding the plugin fails", func() {
				BeforeEach(func() {
					netConfigList.Plugins[1].Network.Type = "does-not-exist"
				})

				It("returns the error", func() {
					err := cniConfig.DelNetworkList(netConfigList, runtimeConfig)
					Expect(err).To(MatchError(ContainSubstring(`failed to find plugin "does-not-exist"`)))
				})
			})

			Context("when the plugin errors", func() {
				BeforeEach(func() {
					plugins[1].debug.ReportError = "plugin error: banana"
					Expect(plugins[1].debug.WriteDebug(plugins[1].debugFilePath)).To(Succeed())
				})
				It("unmarshals and returns the error", func() {
					err := cniConfig.DelNetworkList(netConfigList, runtimeConfig)
					Expect(err).To(MatchError("plugin error: banana"))
				})
			})

			Context("when the cached result is invalid", func() {
				It("returns an error", func() {
					cacheFile := resultCacheFilePath(cacheDirPath, netConfigList.Name, runtimeConfig.ContainerID)
					err := os.MkdirAll(filepath.Dir(cacheFile), 0700)
					Expect(err).NotTo(HaveOccurred())
					err = ioutil.WriteFile(cacheFile, []byte("adfadsfasdfasfdsafaf"), 0600)
					Expect(err).NotTo(HaveOccurred())

					err = cniConfig.DelNetworkList(netConfigList, runtimeConfig)
					Expect(err).To(MatchError("failed to get network 'some-list' cached result: decoding version from network config: invalid character 'a' looking for beginning of value"))
				})
			})
		})

	})
})