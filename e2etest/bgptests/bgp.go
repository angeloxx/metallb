/*
Copyright 2016 The Kubernetes Authors.
Copyright 2021 The MetalLB Authors.

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
// https://github.com/kubernetes/kubernetes/blob/92aff21558831b829fbc8cbca3d52edc80c01aa3/test/e2e/network/loadbalancer.go#L878

package bgptests

import (
	"context"
	"fmt"
	"time"

	"go.universe.tf/metallb/e2etest/pkg/config"
	"go.universe.tf/metallb/e2etest/pkg/executor"
	"go.universe.tf/metallb/e2etest/pkg/k8s"
	"go.universe.tf/metallb/e2etest/pkg/metallb"
	"go.universe.tf/metallb/e2etest/pkg/metrics"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	"go.universe.tf/metallb/e2etest/pkg/frr"
	frrconfig "go.universe.tf/metallb/e2etest/pkg/frr/config"
	frrcontainer "go.universe.tf/metallb/e2etest/pkg/frr/container"
	testservice "go.universe.tf/metallb/e2etest/pkg/service"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
)

const (
	v4PoolAddresses = "192.168.10.0/24"
	v6PoolAddresses = "fc00:f853:0ccd:e799::/124"
	CommunityNoAdv  = "65535:65282" // 0xFFFFFF02: NO_ADVERTISE
	IPLocalPref     = uint32(300)
)

var ConfigUpdater config.Updater

var _ = ginkgo.Describe("BGP", func() {
	var cs clientset.Interface
	var f *framework.Framework

	ginkgo.AfterEach(func() {
		if ginkgo.CurrentGinkgoTestDescription().Failed {
			for _, c := range FRRContainers {
				dump, err := frr.RawDump(c, "/etc/frr/bgpd.conf", "/tmp/frr.log", "/etc/frr/daemons")
				framework.Logf("External frr dump for %s:\n%s\nerrors:%v", c.Name, dump, err)
			}

			speakerPods, err := metallb.SpeakerPods(cs)
			framework.ExpectNoError(err)
			for _, pod := range speakerPods {
				podExec := executor.ForPod(pod.Namespace, pod.Name, "frr")
				dump, err := frr.RawDump(podExec, "/etc/frr/frr.conf", "/etc/frr/frr.log")
				framework.Logf("External frr dump for pod %s\n%s %v", pod.Name, dump, err)
			}
			k8s.DescribeSvc(f.Namespace.Name)
		}
	})

	ginkgo.AfterEach(func() {
		ginkgo.By("Clearing the previous configuration")
		// Clean previous configuration.
		err := ConfigUpdater.Clean()
		framework.ExpectNoError(err)

		for _, c := range FRRContainers {
			err := c.UpdateBGPConfigFile(frrconfig.Empty)
			framework.ExpectNoError(err)
		}
	})

	f = framework.NewDefaultFramework("bgp")

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
	})

	table.DescribeTable("A service of protocol load balancer should work with", func(pairingIPFamily, setProtocoltest string, poolAddresses []string, tweak testservice.Tweak) {
		var allNodes *corev1.NodeList
		configData := config.File{
			Pools: []config.AddressPool{
				{
					Name:      "bgp-test",
					Protocol:  config.BGP,
					Addresses: poolAddresses,
				},
			},
			Peers: metallb.PeersForContainers(FRRContainers, pairingIPFamily),
		}
		for _, c := range FRRContainers {
			err := frrcontainer.PairWithNodes(cs, c, pairingIPFamily)
			framework.ExpectNoError(err)
		}

		err := ConfigUpdater.Update(configData)
		framework.ExpectNoError(err)

		for _, c := range FRRContainers {
			validateFRRPeeredWithNodes(cs, c, pairingIPFamily)
		}

		allNodes, err = cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		framework.ExpectNoError(err)

		if setProtocoltest == "ExternalTrafficPolicyCluster" {
			svc, _ := testservice.CreateWithBackend(cs, f.Namespace.Name, "external-local-lb", tweak)

			defer func() {
				err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}()

			for _, c := range FRRContainers {
				validateService(cs, svc, allNodes.Items, c)
			}
		}

		if setProtocoltest == "ExternalTrafficPolicyLocal" {
			svc, jig := testservice.CreateWithBackend(cs, f.Namespace.Name, "external-local-lb", tweak)
			err = jig.Scale(2)
			framework.ExpectNoError(err)

			epNodes, err := jig.ListNodesWithEndpoint() // Only nodes with an endpoint should be advertising the IP
			framework.ExpectNoError(err)

			defer func() {
				err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}()

			for _, c := range FRRContainers {
				validateService(cs, svc, epNodes, c)
			}
		}

		if setProtocoltest == "CheckSpeakerFRRPodRunning" {
			for _, c := range FRRContainers {
				frrIsPairedOnPods(cs, c, pairingIPFamily)
			}
		}
	},
		table.Entry("IPV4 - ExternalTrafficPolicyCluster", "ipv4", "ExternalTrafficPolicyCluster", []string{v4PoolAddresses}, testservice.TrafficPolicyCluster),
		table.Entry("IPV4 - ExternalTrafficPolicyLocal", "ipv4", "ExternalTrafficPolicyLocal", []string{v4PoolAddresses}, testservice.TrafficPolicyLocal),
		table.Entry("IPV4 - FRR running in the speaker POD", "ipv4", "CheckSpeakerFRRPodRunning", []string{v4PoolAddresses}, testservice.TrafficPolicyLocal),
		table.Entry("IPV6 - ExternalTrafficPolicyCluster", "ipv6", "ExternalTrafficPolicyCluster", []string{v6PoolAddresses}, testservice.TrafficPolicyCluster),
		table.Entry("IPV6 - ExternalTrafficPolicyLocal", "ipv6", "ExternalTrafficPolicyLocal", []string{v6PoolAddresses}, testservice.TrafficPolicyLocal),
		table.Entry("IPV6 - FRR running in the speaker POD", "ipv6", "CheckSpeakerFRRPodRunning", []string{v6PoolAddresses}, testservice.TrafficPolicyLocal),
		table.Entry("DUALSTACK - ExternalTrafficPolicyCluster", "dual", "ExternalTrafficPolicyCluster", []string{v4PoolAddresses, v6PoolAddresses},
			func(svc *corev1.Service) {
				testservice.TrafficPolicyCluster(svc)
				testservice.DualStack(svc)
			}),
		table.Entry("DUALSTACK - ExternalTrafficPolicyLocal", "dual", "ExternalTrafficPolicyLocal", []string{v4PoolAddresses, v6PoolAddresses},
			func(svc *corev1.Service) {
				testservice.TrafficPolicyLocal(svc)
				testservice.DualStack(svc)
			}),
		table.Entry("DUALSTACK - ExternalTrafficPolicyCluster - force V6 only", "dual", "ExternalTrafficPolicyCluster", []string{v4PoolAddresses, v6PoolAddresses},
			func(svc *corev1.Service) {
				testservice.TrafficPolicyCluster(svc)
				testservice.ForceV6(svc)
			}),
	)

	ginkgo.Context("metrics", func() {
		var controllerPod *corev1.Pod
		var speakerPods []*corev1.Pod

		ginkgo.BeforeEach(func() {
			var err error
			controllerPod, err = metallb.ControllerPod(cs)
			framework.ExpectNoError(err)
			speakerPods, err = metallb.SpeakerPods(cs)
			framework.ExpectNoError(err)
		})

		table.DescribeTable("should be exposed by the controller", func(ipFamily, poolAddress string, addressTotal int) {
			poolName := "bgp-test"

			var peerAddrs []string
			for _, c := range FRRContainers {
				address := c.Ipv4
				if ipFamily == "ipv6" {
					address = c.Ipv6
				}
				peerAddrs = append(peerAddrs, address+fmt.Sprintf(":%d", c.RouterConfig.BGPPort))
			}

			configData := config.File{
				Pools: []config.AddressPool{
					{
						Name:      poolName,
						Protocol:  config.BGP,
						Addresses: []string{poolAddress},
					},
				},
				Peers: metallb.PeersForContainers(FRRContainers, ipFamily),
			}
			for _, c := range FRRContainers {
				err := frrcontainer.PairWithNodes(cs, c, ipFamily)
				framework.ExpectNoError(err)
			}

			err := ConfigUpdater.Update(configData)
			framework.ExpectNoError(err)

			for _, c := range FRRContainers {
				validateFRRPeeredWithNodes(cs, c, ipFamily)
			}

			ginkgo.By("checking the metrics when no service is added")
			Eventually(func() error {
				controllerMetrics, err := metrics.ForPod(controllerPod, controllerPod, metallb.Namespace)
				if err != nil {
					return err
				}
				err = metrics.ValidateGaugeValue(0, "metallb_allocator_addresses_in_use_total", map[string]string{"pool": poolName}, controllerMetrics)
				if err != nil {
					return err
				}
				err = metrics.ValidateGaugeValue(addressTotal, "metallb_allocator_addresses_total", map[string]string{"pool": poolName}, controllerMetrics)
				if err != nil {
					return err
				}
				return nil
			}, 2*time.Minute, 1*time.Second).Should(BeNil())

			for _, speaker := range speakerPods {
				ginkgo.By(fmt.Sprintf("checking speaker %s", speaker.Name))

				Eventually(func() error {
					speakerMetrics, err := metrics.ForPod(controllerPod, speaker, metallb.Namespace)
					if err != nil {
						return err
					}
					for _, addr := range peerAddrs {
						err = metrics.ValidateGaugeValue(1, "metallb_bgp_session_up", map[string]string{"peer": addr}, speakerMetrics)
						if err != nil {
							return err
						}
						err = metrics.ValidateGaugeValue(0, "metallb_bgp_announced_prefixes_total", map[string]string{"peer": addr}, speakerMetrics)
						if err != nil {
							return err
						}
					}
					return nil
				}, 2*time.Minute, 1*time.Second).Should(BeNil())
			}

			ginkgo.By("creating a service")
			svc, _ := testservice.CreateWithBackend(cs, f.Namespace.Name, "external-local-lb", testservice.TrafficPolicyCluster) // Is a sleep required here?
			defer func() {
				err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}()

			ginkgo.By("checking the metrics when a service is added")
			Eventually(func() error {
				controllerMetrics, err := metrics.ForPod(controllerPod, controllerPod, metallb.Namespace)
				if err != nil {
					return err
				}
				err = metrics.ValidateGaugeValue(1, "metallb_allocator_addresses_in_use_total", map[string]string{"pool": poolName}, controllerMetrics)
				if err != nil {
					return err
				}
				return nil
			}, 2*time.Minute, 1*time.Second).Should(BeNil())

			for _, speaker := range speakerPods {
				ginkgo.By(fmt.Sprintf("checking speaker %s", speaker.Name))

				Eventually(func() error {
					speakerMetrics, err := metrics.ForPod(controllerPod, speaker, metallb.Namespace)
					if err != nil {
						return err
					}
					for _, addr := range peerAddrs {
						err = metrics.ValidateGaugeValue(1, "metallb_bgp_session_up", map[string]string{"peer": addr}, speakerMetrics)
						if err != nil {
							return err
						}

						err = metrics.ValidateGaugeValue(1, "metallb_bgp_announced_prefixes_total", map[string]string{"peer": addr}, speakerMetrics)
						if err != nil {
							return err
						}

						err = metrics.ValidateCounterValue(1, "metallb_bgp_updates_total", map[string]string{"peer": addr}, speakerMetrics)
						if err != nil {
							return err
						}
					}

					err = metrics.ValidateGaugeValue(1, "metallb_speaker_announced", map[string]string{"node": speaker.Spec.NodeName, "protocol": "bgp", "service": fmt.Sprintf("%s/%s", f.Namespace.Name, svc.Name)}, speakerMetrics)
					if err != nil {
						return err
					}
					return nil
				}, 2*time.Minute, 1*time.Second).Should(BeNil())
			}
		},
			table.Entry("IPV4 - Checking service", "ipv4", v4PoolAddresses, 256),
			table.Entry("IPV6 - Checking service", "ipv6", v6PoolAddresses, 16))
	})

	ginkgo.Context("validate different AddressPools for type=Loadbalancer", func() {
		ginkgo.AfterEach(func() {
			// Clean previous configuration.
			err := ConfigUpdater.Clean()
			framework.ExpectNoError(err)
		})

		table.DescribeTable("set different AddressPools ranges modes", func(addressPools []config.AddressPool, pairingFamily string, tweak testservice.Tweak) {
			configData := config.File{
				Peers: metallb.PeersForContainers(FRRContainers, pairingFamily),
				Pools: addressPools,
			}
			for _, c := range FRRContainers {
				err := frrcontainer.PairWithNodes(cs, c, pairingFamily)
				framework.ExpectNoError(err)
			}

			err := ConfigUpdater.Update(configData)
			framework.ExpectNoError(err)

			for _, c := range FRRContainers {
				validateFRRPeeredWithNodes(cs, c, pairingFamily)
			}

			svc, _ := testservice.CreateWithBackend(cs, f.Namespace.Name, "external-local-lb", tweak)

			defer func() {
				err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}()

			for _, i := range svc.Status.LoadBalancer.Ingress {
				ginkgo.By("validate LoadBalancer IP is in the AddressPool range")
				ingressIP := e2eservice.GetIngressPoint(&i)
				err = config.ValidateIPInRange(addressPools, ingressIP)
				framework.ExpectNoError(err)
			}

			allNodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			framework.ExpectNoError(err)

			for _, c := range FRRContainers {
				validateService(cs, svc, allNodes.Items, c)
			}
		},
			table.Entry("IPV4 - test AddressPool defined by address range", []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						"192.168.10.0-192.168.10.18",
					},
				}}, "ipv4", testservice.TrafficPolicyCluster,
			),
			table.Entry("IPV4 - test AddressPool defined by network prefix", []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						"192.168.10.0/24",
					},
				}}, "ipv4", testservice.TrafficPolicyCluster,
			),
			table.Entry("IPV6 - test AddressPool defined by address range", []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						"fc00:f853:0ccd:e799::0-fc00:f853:0ccd:e799::18",
					},
				}}, "ipv6", testservice.TrafficPolicyCluster,
			),
			table.Entry("IPV6 - test AddressPool defined by network prefix", []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						"fc00:f853:0ccd:e799::/124",
					},
				}}, "ipv6", testservice.TrafficPolicyCluster,
			),
			table.Entry("DUALSTACK - test AddressPool defined by address range", []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						"192.168.10.0-192.168.10.18",
						"fc00:f853:0ccd:e799::0-fc00:f853:0ccd:e799::18",
					},
				}}, "dual", testservice.TrafficPolicyCluster,
			),
			table.Entry("DUALSTACK - test AddressPool defined by network prefix", []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						"192.168.10.0/24",
						"fc00:f853:0ccd:e799::/124",
					},
				}}, "dual", testservice.TrafficPolicyCluster,
			),
		)
	})

	ginkgo.Context("BFD", func() {
		table.DescribeTable("should work with the given bfd profile", func(bfd config.BfdProfile, pairingFamily string, poolAddresses []string, tweak testservice.Tweak) {
			configData := config.File{
				Pools: []config.AddressPool{
					{
						Name:      "bfd-test",
						Protocol:  config.BGP,
						Addresses: poolAddresses,
					},
				},
				Peers:       metallb.WithBFD(metallb.PeersForContainers(FRRContainers, pairingFamily), bfd.Name),
				BFDProfiles: []config.BfdProfile{bfd},
			}
			err := ConfigUpdater.Update(configData)
			framework.ExpectNoError(err)

			for _, c := range FRRContainers {
				err := frrcontainer.PairWithNodes(cs, c, pairingFamily, func(container *frrcontainer.FRR) {
					container.NeighborConfig.BFDEnabled = true
				})
				framework.ExpectNoError(err)
			}

			svc, _ := testservice.CreateWithBackend(cs, f.Namespace.Name, "external-local-lb", tweak)
			defer func() {
				err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			}()

			allNodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			framework.ExpectNoError(err)

			for _, c := range FRRContainers {
				validateFRRPeeredWithNodes(cs, c, pairingFamily)
			}
			for _, c := range FRRContainers {
				validateService(cs, svc, allNodes.Items, c)
			}

			Eventually(func() error {
				for _, c := range FRRContainers {
					bfdPeers, err := frr.BFDPeers(c.Executor)
					if err != nil {
						return err
					}
					err = frr.BFDPeersMatchNodes(allNodes.Items, bfdPeers, pairingFamily)
					if err != nil {
						return err
					}
					for _, peerConfig := range bfdPeers {
						toCompare := config.BFDProfileWithDefaults(bfd, peerConfig.Multihop)
						ginkgo.By(fmt.Sprintf("Checking bfd parameters on %s", peerConfig.Peer))
						err := checkBFDConfigPropagated(toCompare, peerConfig)
						if err != nil {
							return err
						}
					}
				}
				return nil
			}, 4*time.Minute, 1*time.Second).Should(BeNil())

		},
			table.Entry("IPV4 - default",
				config.BfdProfile{
					Name: "bar",
				}, "ipv4", []string{v4PoolAddresses}, testservice.TrafficPolicyCluster),
			table.Entry("IPV4 - full params",
				config.BfdProfile{
					Name:             "full1",
					ReceiveInterval:  uint32Ptr(60),
					TransmitInterval: uint32Ptr(61),
					EchoInterval:     uint32Ptr(62),
					EchoMode:         boolPtr(false),
					PassiveMode:      boolPtr(false),
					MinimumTTL:       uint32Ptr(254),
				}, "ipv4", []string{v4PoolAddresses}, testservice.TrafficPolicyCluster),
			table.Entry("IPV4 - echo mode enabled",
				config.BfdProfile{
					Name:             "echo",
					ReceiveInterval:  uint32Ptr(80),
					TransmitInterval: uint32Ptr(81),
					EchoInterval:     uint32Ptr(82),
					EchoMode:         boolPtr(true),
					PassiveMode:      boolPtr(false),
					MinimumTTL:       uint32Ptr(254),
				}, "ipv4", []string{v4PoolAddresses}, testservice.TrafficPolicyCluster),
			table.Entry("IPV6 - default",
				config.BfdProfile{
					Name: "bar",
				}, "ipv6", []string{v6PoolAddresses}, testservice.TrafficPolicyCluster),
			table.Entry("IPV6 - full params",
				config.BfdProfile{
					Name:             "full1",
					ReceiveInterval:  uint32Ptr(60),
					TransmitInterval: uint32Ptr(61),
					EchoInterval:     uint32Ptr(62),
					EchoMode:         boolPtr(false),
					PassiveMode:      boolPtr(false),
					MinimumTTL:       uint32Ptr(254),
				}, "ipv6", []string{v6PoolAddresses}, testservice.TrafficPolicyCluster),
			table.Entry("IPV6 - echo mode enabled",
				config.BfdProfile{
					Name:             "echo",
					ReceiveInterval:  uint32Ptr(80),
					TransmitInterval: uint32Ptr(81),
					EchoInterval:     uint32Ptr(82),
					EchoMode:         boolPtr(true),
					PassiveMode:      boolPtr(false),
					MinimumTTL:       uint32Ptr(254),
				}, "ipv6", []string{v6PoolAddresses}, testservice.TrafficPolicyCluster),
			table.Entry("DUALSTACK - full params",
				config.BfdProfile{
					Name:             "full1",
					ReceiveInterval:  uint32Ptr(60),
					TransmitInterval: uint32Ptr(61),
					EchoInterval:     uint32Ptr(62),
					EchoMode:         boolPtr(false),
					PassiveMode:      boolPtr(false),
					MinimumTTL:       uint32Ptr(254),
				}, "dual", []string{v4PoolAddresses, v6PoolAddresses}, func(svc *corev1.Service) {
					testservice.TrafficPolicyCluster(svc)
					testservice.DualStack(svc)
				}),
		)

		table.DescribeTable("metrics", func(bfd config.BfdProfile, pairingFamily string, poolAddresses []string) {
			configData := config.File{
				Pools: []config.AddressPool{
					{
						Name:      "bfd-test",
						Protocol:  config.BGP,
						Addresses: poolAddresses,
					},
				},
				Peers:       metallb.WithBFD(metallb.PeersForContainers(FRRContainers, pairingFamily), bfd.Name),
				BFDProfiles: []config.BfdProfile{bfd},
			}
			err := ConfigUpdater.Update(configData)
			framework.ExpectNoError(err)

			for _, c := range FRRContainers {
				err := frrcontainer.PairWithNodes(cs, c, pairingFamily, func(container *frrcontainer.FRR) {
					container.NeighborConfig.BFDEnabled = true
				})
				framework.ExpectNoError(err)
			}

			for _, c := range FRRContainers {
				validateFRRPeeredWithNodes(cs, c, pairingFamily)
			}

			ginkgo.By("checking metrics")
			controllerPod, err := metallb.ControllerPod(cs)
			framework.ExpectNoError(err)
			speakerPods, err := metallb.SpeakerPods(cs)
			framework.ExpectNoError(err)

			var peers []struct {
				addr     string
				multihop bool
			}

			for _, c := range FRRContainers {
				address := c.Ipv4
				if pairingFamily == "ipv6" {
					address = c.Ipv6
				}

				peers = append(peers, struct {
					addr     string
					multihop bool
				}{
					address,
					c.NeighborConfig.MultiHop,
				},
				)
			}

			for _, speaker := range speakerPods {
				ginkgo.By(fmt.Sprintf("checking speaker %s", speaker.Name))

				Eventually(func() error {
					speakerMetrics, err := metrics.ForPod(controllerPod, speaker, metallb.Namespace)
					if err != nil {
						return err
					}

					for _, peer := range peers {
						err = metrics.ValidateGaugeValue(1, "metallb_bfd_session_up", map[string]string{"peer": peer.addr}, speakerMetrics)
						if err != nil {
							return err
						}

						err = metrics.ValidateCounterValue(1, "metallb_bfd_control_packet_input", map[string]string{"peer": peer.addr}, speakerMetrics)
						if err != nil {
							return err
						}

						err = metrics.ValidateCounterValue(1, "metallb_bfd_control_packet_output", map[string]string{"peer": peer.addr}, speakerMetrics)
						if err != nil {
							return err
						}

						err = metrics.ValidateGaugeValue(0, "metallb_bfd_session_down_events", map[string]string{"peer": peer.addr}, speakerMetrics)
						if err != nil {
							return err
						}

						err = metrics.ValidateCounterValue(1, "metallb_bfd_session_up_events", map[string]string{"peer": peer.addr}, speakerMetrics)
						if err != nil {
							return err
						}

						err = metrics.ValidateCounterValue(1, "metallb_bfd_zebra_notifications", map[string]string{"peer": peer.addr}, speakerMetrics)
						if err != nil {
							return err
						}

						if bfd.EchoMode != nil && *bfd.EchoMode {
							echoVal := 1
							if peer.multihop {
								echoVal = 0
							}
							err = metrics.ValidateCounterValue(echoVal, "metallb_bfd_echo_packet_input", map[string]string{"peer": peer.addr}, speakerMetrics)
							if err != nil {
								return err
							}

							err = metrics.ValidateCounterValue(echoVal, "metallb_bfd_echo_packet_output", map[string]string{"peer": peer.addr}, speakerMetrics)
							if err != nil {
								return err
							}
						}
					}
					return nil
				}, 2*time.Minute, 1*time.Second).Should(BeNil())
			}

			ginkgo.By("disabling BFD in external FRR containers")
			for _, c := range FRRContainers {
				err := frrcontainer.PairWithNodes(cs, c, pairingFamily, func(container *frrcontainer.FRR) {
					container.NeighborConfig.BFDEnabled = false
				})
				framework.ExpectNoError(err)
			}

			ginkgo.By("validating session down metrics")
			for _, speaker := range speakerPods {
				ginkgo.By(fmt.Sprintf("checking speaker %s", speaker.Name))

				Eventually(func() error {
					speakerMetrics, err := metrics.ForPod(controllerPod, speaker, metallb.Namespace)
					if err != nil {
						return err
					}

					for _, peer := range peers {
						err = metrics.ValidateGaugeValue(0, "metallb_bfd_session_up", map[string]string{"peer": peer.addr}, speakerMetrics)
						if err != nil {
							return err
						}

						err = metrics.ValidateCounterValue(1, "metallb_bfd_session_down_events", map[string]string{"peer": peer.addr}, speakerMetrics)
						if err != nil {
							return err
						}
					}
					return nil
				}, 2*time.Minute, 1*time.Second).Should(BeNil())
			}
		},
			table.Entry("IPV4 - default",
				config.BfdProfile{
					Name: "bar",
				}, "ipv4", []string{v4PoolAddresses}),
			table.Entry("IPV4 - echo mode enabled",
				config.BfdProfile{
					Name:             "echo",
					ReceiveInterval:  uint32Ptr(80),
					TransmitInterval: uint32Ptr(81),
					EchoInterval:     uint32Ptr(82),
					EchoMode:         boolPtr(true),
					PassiveMode:      boolPtr(false),
					MinimumTTL:       uint32Ptr(254),
				}, "ipv4", []string{v4PoolAddresses}),
			table.Entry("IPV6 - default",
				config.BfdProfile{
					Name: "bar",
				}, "ipv6", []string{v6PoolAddresses}),
			table.Entry("IPV6 - echo mode enabled",
				config.BfdProfile{
					Name:             "echo",
					ReceiveInterval:  uint32Ptr(80),
					TransmitInterval: uint32Ptr(81),
					EchoInterval:     uint32Ptr(82),
					EchoMode:         boolPtr(true),
					PassiveMode:      boolPtr(false),
					MinimumTTL:       uint32Ptr(254),
				}, "ipv6", []string{v6PoolAddresses}),
			table.Entry("DUALSTACK - full params",
				config.BfdProfile{
					Name:             "full1",
					ReceiveInterval:  uint32Ptr(60),
					TransmitInterval: uint32Ptr(61),
					EchoInterval:     uint32Ptr(62),
					EchoMode:         boolPtr(false),
					PassiveMode:      boolPtr(false),
					MinimumTTL:       uint32Ptr(254),
				}, "dual", []string{v4PoolAddresses, v6PoolAddresses}),
		)
	})

	ginkgo.Context("validate configuration changes", func() {
		table.DescribeTable("should work after subsequent configuration updates", func(addressRange string, ipFamily string) {
			var services []*corev1.Service
			var servicesIngressIP []string
			var pools []config.AddressPool

			allNodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
			framework.ExpectNoError(err)

			for i := 0; i < 2; i++ {
				ginkgo.By(fmt.Sprintf("configure addresspool number %d", i+1))
				firstIP, err := config.GetIPFromRangeByIndex(addressRange, i*10+1)
				framework.ExpectNoError(err)
				lastIP, err := config.GetIPFromRangeByIndex(addressRange, i*10+10)
				framework.ExpectNoError(err)
				addressesRange := fmt.Sprintf("%s-%s", firstIP, lastIP)
				pool := config.AddressPool{
					Name:     fmt.Sprintf("test-addresspool%d", i+1),
					Protocol: config.BGP,
					Addresses: []string{
						addressesRange,
					},
				}
				pools = append(pools, pool)

				configData := config.File{
					Pools: pools,
					Peers: metallb.PeersForContainers(FRRContainers, ipFamily),
				}

				for _, c := range FRRContainers {
					err := frrcontainer.PairWithNodes(cs, c, ipFamily)
					framework.ExpectNoError(err)
				}

				err = ConfigUpdater.Update(configData)
				framework.ExpectNoError(err)

				for _, c := range FRRContainers {
					validateFRRPeeredWithNodes(cs, c, ipFamily)
				}

				ginkgo.By(fmt.Sprintf("configure service number %d", i+1))
				svc, _ := testservice.CreateWithBackend(cs, f.Namespace.Name, fmt.Sprintf("svc%d", i+1), testservice.TrafficPolicyCluster, func(svc *corev1.Service) {
					svc.Annotations = map[string]string{"metallb.universe.tf/address-pool": fmt.Sprintf("test-addresspool%d", i+1)}
				})

				defer func() {
					err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
					framework.ExpectNoError(err)
				}()

				ginkgo.By("validate LoadBalancer IP is in the AddressPool range")
				ingressIP := e2eservice.GetIngressPoint(
					&svc.Status.LoadBalancer.Ingress[0])
				err = config.ValidateIPInRange([]config.AddressPool{pool}, ingressIP)
				framework.ExpectNoError(err)

				services = append(services, svc)
				servicesIngressIP = append(servicesIngressIP, ingressIP)

				for j := 0; j <= i; j++ {
					ginkgo.By(fmt.Sprintf("validate service %d IP didn't change", j+1))
					ip := e2eservice.GetIngressPoint(&services[j].Status.LoadBalancer.Ingress[0])
					framework.ExpectEqual(ip, servicesIngressIP[j])

					ginkgo.By(fmt.Sprintf("checking connectivity of service %d to its external VIP", j+1))
					for _, c := range FRRContainers {
						validateService(cs, svc, allNodes.Items, c)
					}
				}
			}
		},
			table.Entry("IPV4", "192.168.10.0/24", "ipv4"),
			table.Entry("IPV6", "fc00:f853:0ccd:e799::/116", "ipv6"))

		table.DescribeTable("configure peers one by one and validate FRR paired with nodes", func(ipFamily string) {
			for i, c := range FRRContainers {
				ginkgo.By("configure peer")

				configData := config.File{
					Peers: metallb.PeersForContainers([]*frrcontainer.FRR{c}, ipFamily),
				}
				err := ConfigUpdater.Update(configData)
				framework.ExpectNoError(err)

				err = frrcontainer.PairWithNodes(cs, c, ipFamily)
				framework.ExpectNoError(err)

				validateFRRPeeredWithNodes(cs, FRRContainers[i], ipFamily)
			}
		},
			table.Entry("IPV4", "ipv4"),
			table.Entry("IPV6", "ipv6"))

		table.DescribeTable("configure bgp community and verify it gets propagated",
			func(addressPools []config.AddressPool, ipFamily string) {
				configData := config.File{
					Peers: metallb.PeersForContainers(FRRContainers, ipFamily),
					Pools: addressPools,
				}
				for _, c := range FRRContainers {
					err := frrcontainer.PairWithNodes(cs, c, ipFamily)
					framework.ExpectNoError(err)
				}

				err := ConfigUpdater.Update(configData)
				framework.ExpectNoError(err)

				for _, c := range FRRContainers {
					validateFRRPeeredWithNodes(cs, c, ipFamily)
				}

				svc, _ := testservice.CreateWithBackend(cs, f.Namespace.Name, "external-local-lb", testservice.TrafficPolicyCluster)

				defer func() {
					err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
					framework.ExpectNoError(err)
				}()

				for _, i := range svc.Status.LoadBalancer.Ingress {
					ginkgo.By("validate LoadBalancer IP is in the AddressPool range")
					ingressIP := e2eservice.GetIngressPoint(&i)
					err = config.ValidateIPInRange(addressPools, ingressIP)
					framework.ExpectNoError(err)
				}

				allNodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
				framework.ExpectNoError(err)

				for _, c := range FRRContainers {
					validateService(cs, svc, allNodes.Items, c)
					Eventually(func() error {
						return frr.ContainsCommunity(c, "no-advertise")
					}, 4*time.Minute, 1*time.Second).Should(BeNil())
				}
			},
			table.Entry("IPV4", []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						"192.168.10.0/24",
					},
					BGPAdvertisements: []config.BgpAdvertisement{
						{
							Communities: []string{
								CommunityNoAdv,
							},
						},
					},
				}}, "ipv4"),
			table.Entry("IPV6", []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						"fc00:f853:0ccd:e799::0-fc00:f853:0ccd:e799::18",
					},
					BGPAdvertisements: []config.BgpAdvertisement{
						{
							Communities: []string{
								CommunityNoAdv,
							},
						},
					},
				}}, "ipv6"))

		table.DescribeTable("configure bgp local-preference and verify it gets propagated",
			func(poolAddresses []string, ipFamily string, localPref uint32) {
				configData := config.File{
					Pools: []config.AddressPool{
						{
							Name:      "bgp-test",
							Protocol:  config.BGP,
							Addresses: poolAddresses,
							BGPAdvertisements: []config.BgpAdvertisement{
								{
									LocalPref: uint32Ptr(localPref),
								},
							},
						},
					},
					Peers: metallb.PeersForContainers(FRRContainers, ipFamily),
				}
				for _, c := range FRRContainers {
					err := frrcontainer.PairWithNodes(cs, c, ipFamily)
					framework.ExpectNoError(err)
				}

				err := ConfigUpdater.Update(configData)
				framework.ExpectNoError(err)

				for _, c := range FRRContainers {
					validateFRRPeeredWithNodes(cs, c, ipFamily)
				}

				svc, _ := testservice.CreateWithBackend(cs, f.Namespace.Name, "external-local-lb", testservice.TrafficPolicyCluster)

				defer func() {
					err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
					framework.ExpectNoError(err)
				}()

				allNodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
				framework.ExpectNoError(err)

				for _, c := range FRRContainers {
					validateService(cs, svc, allNodes.Items, c)
				}
				// LocalPref check is only valid for iBGP sessions
				for _, c := range FRRContainers {
					if c.Name == "frrIBGP" {
						Eventually(func() error {
							return frr.RoutesMatchLocalPref(c, ipFamily, localPref)
						}, 4*time.Minute, 1*time.Second).Should(BeNil())
					}
				}
			},
			table.Entry("IPV4", []string{v4PoolAddresses}, "ipv4", IPLocalPref),
			table.Entry("IPV6", []string{v6PoolAddresses}, "ipv6", IPLocalPref))
	})

	table.DescribeTable("MetalLB FRR rejects any routes advertised by any neighbor", func(addressesRange, pairingIPFamily, toInject string) {
		configData := config.File{
			Pools: []config.AddressPool{
				{
					Name:     "bgp-test",
					Protocol: config.BGP,
					Addresses: []string{
						addressesRange,
					},
				},
			},
			Peers: metallb.PeersForContainers(FRRContainers, pairingIPFamily),
		}
		neighborAnnounce := func(frr *frrcontainer.FRR) {
			frr.NeighborConfig.ToAdvertise = toInject
		}

		for _, c := range FRRContainers {
			err := frrcontainer.PairWithNodes(cs, c, pairingIPFamily, neighborAnnounce)
			framework.ExpectNoError(err)
		}

		err := ConfigUpdater.Update(configData)
		framework.ExpectNoError(err)

		for _, c := range FRRContainers {
			validateFRRPeeredWithNodes(cs, c, pairingIPFamily)
		}
		speakerPods, err := metallb.SpeakerPods(cs)
		framework.ExpectNoError(err)

		checkRoutesInjected := func() error {
			for _, pod := range speakerPods {
				podExec := executor.ForPod(pod.Namespace, pod.Name, "frr")
				routes, frrRoutesV6, err := frr.Routes(podExec)
				framework.ExpectNoError(err)

				if pairingIPFamily == "ipv6" {
					routes = frrRoutesV6
				}

				for _, route := range routes {
					if route.Destination.String() == toInject {
						return fmt.Errorf("Found %s in %s routes", toInject, pod.Name)
					}
				}
			}
			return nil
		}

		Consistently(checkRoutesInjected, 30*time.Second, 1*time.Second).ShouldNot(HaveOccurred())
		svc, _ := testservice.CreateWithBackend(cs, f.Namespace.Name, "external-local-lb")

		defer func() {
			err := cs.CoreV1().Services(svc.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err)
		}()

		Consistently(checkRoutesInjected, 30*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

	},
		table.Entry("IPV4", "192.168.10.0/24", "ipv4", "172.16.1.10/32"),
		table.Entry("IPV6", "fc00:f853:0ccd:e799::/116", "ipv6", "fc00:f853:ccd:e800::1/128"),
	)

})

func uint32Ptr(n uint32) *uint32 {
	return &n
}

func boolPtr(b bool) *bool {
	return &b
}