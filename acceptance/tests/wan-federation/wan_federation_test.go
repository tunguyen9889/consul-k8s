// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package wanfederation

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/hashicorp/consul-k8s/acceptance/framework/consul"
	"github.com/hashicorp/consul-k8s/acceptance/framework/environment"
	"github.com/hashicorp/consul-k8s/acceptance/framework/helpers"
	"github.com/hashicorp/consul-k8s/acceptance/framework/k8s"
	"github.com/hashicorp/consul-k8s/acceptance/framework/logger"
	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const StaticClientName = "static-client"

// Test that Connect and wan federation over mesh gateways work in a default installation
// i.e. without ACLs because TLS is required for WAN federation over mesh gateways.
func TestWANFederation(t *testing.T) {
	cases := []struct {
		name   string
		secure bool
	}{
		{
			name:   "secure",
			secure: true,
		},
		{
			name:   "default",
			secure: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {

			env := suite.Environment()
			cfg := suite.Config()

			if cfg.UseKind {
				t.Skipf("skipping wan federation tests as they currently fail on Kind even though they work on other clouds.")
			}

			primaryContext := env.DefaultContext(t)
			secondaryContext := env.Context(t, environment.SecondaryContextName)

			primaryHelmValues := map[string]string{
				"global.datacenter": "dc1",

				"global.tls.enabled":   "true",
				"global.tls.httpsOnly": strconv.FormatBool(c.secure),

				"global.federation.enabled":                "true",
				"global.federation.createFederationSecret": "true",

				"global.acls.manageSystemACLs":       strconv.FormatBool(c.secure),
				"global.acls.createReplicationToken": strconv.FormatBool(c.secure),

				"connectInject.enabled":  "true",
				"connectInject.replicas": "1",

				"meshGateway.enabled":  "true",
				"meshGateway.replicas": "1",
			}

			if cfg.UseKind {
				primaryHelmValues["meshGateway.service.type"] = "NodePort"
				primaryHelmValues["meshGateway.service.nodePort"] = "30000"
			}

			releaseName := helpers.RandomName()

			// Install the primary consul cluster in the default kubernetes context
			primaryConsulCluster := consul.NewHelmCluster(t, primaryHelmValues, primaryContext, cfg, releaseName)
			primaryConsulCluster.Create(t)

			// Get the federation secret from the primary cluster and apply it to secondary cluster
			federationSecretName := fmt.Sprintf("%s-consul-federation", releaseName)
			logger.Logf(t, "retrieving federation secret %s from the primary cluster and applying to the secondary", federationSecretName)
			federationSecret, err := primaryContext.KubernetesClient(t).CoreV1().Secrets(primaryContext.KubectlOptions(t).Namespace).Get(context.Background(), federationSecretName, metav1.GetOptions{})
			require.NoError(t, err)
			federationSecret.ResourceVersion = ""
			_, err = secondaryContext.KubernetesClient(t).CoreV1().Secrets(secondaryContext.KubectlOptions(t).Namespace).Create(context.Background(), federationSecret, metav1.CreateOptions{})
			require.NoError(t, err)

			var k8sAuthMethodHost string
			// When running on kind, the kube API address in kubeconfig will have a localhost address
			// which will not work from inside the container. That's why we need to use the endpoints address instead
			// which will point the node IP.
			if cfg.UseKind {
				// The Kubernetes AuthMethod host is read from the endpoints for the Kubernetes service.
				kubernetesEndpoint, err := secondaryContext.KubernetesClient(t).CoreV1().Endpoints("default").Get(context.Background(), "kubernetes", metav1.GetOptions{})
				require.NoError(t, err)
				k8sAuthMethodHost = fmt.Sprintf("%s:%d", kubernetesEndpoint.Subsets[0].Addresses[0].IP, kubernetesEndpoint.Subsets[0].Ports[0].Port)
			} else {
				k8sAuthMethodHost = k8s.KubernetesAPIServerHostFromOptions(t, secondaryContext.KubectlOptions(t))
			}

			// Create secondary cluster
			secondaryHelmValues := map[string]string{
				"global.datacenter": "dc2",

				"global.tls.enabled":           "true",
				"global.tls.httpsOnly":         "false",
				"global.acls.manageSystemACLs": strconv.FormatBool(c.secure),
				"global.tls.caCert.secretName": federationSecretName,
				"global.tls.caCert.secretKey":  "caCert",
				"global.tls.caKey.secretName":  federationSecretName,
				"global.tls.caKey.secretKey":   "caKey",

				"global.federation.enabled": "true",

				"server.extraVolumes[0].type":          "secret",
				"server.extraVolumes[0].name":          federationSecretName,
				"server.extraVolumes[0].load":          "true",
				"server.extraVolumes[0].items[0].key":  "serverConfigJSON",
				"server.extraVolumes[0].items[0].path": "config.json",

				"connectInject.enabled":  "true",
				"connectInject.replicas": "1",

				"meshGateway.enabled":  "true",
				"meshGateway.replicas": "1",
			}

			if c.secure {
				secondaryHelmValues["global.acls.replicationToken.secretName"] = federationSecretName
				secondaryHelmValues["global.acls.replicationToken.secretKey"] = "replicationToken"
				secondaryHelmValues["global.federation.k8sAuthMethodHost"] = k8sAuthMethodHost
				secondaryHelmValues["global.federation.primaryDatacenter"] = "dc1"
			}

			if cfg.UseKind {
				secondaryHelmValues["meshGateway.service.type"] = "NodePort"
				secondaryHelmValues["meshGateway.service.nodePort"] = "30000"
			}

			// Install the secondary consul cluster in the secondary kubernetes context
			secondaryConsulCluster := consul.NewHelmCluster(t, secondaryHelmValues, secondaryContext, cfg, releaseName)
			secondaryConsulCluster.Create(t)

			primaryClient, _ := primaryConsulCluster.SetupConsulClient(t, c.secure)
			secondaryClient, _ := secondaryConsulCluster.SetupConsulClient(t, c.secure)

			// Verify federation between servers
			logger.Log(t, "verifying federation was successful")
			helpers.VerifyFederation(t, primaryClient, secondaryClient, releaseName, c.secure)

			// Create a ProxyDefaults resource to configure services to use the mesh
			// gateways.
			logger.Log(t, "creating proxy-defaults config")
			kustomizeDir := "../fixtures/bases/mesh-gateway"
			k8s.KubectlApplyK(t, secondaryContext.KubectlOptions(t), kustomizeDir)
			helpers.Cleanup(t, cfg.NoCleanupOnFailure, func() {
				k8s.KubectlDeleteK(t, secondaryContext.KubectlOptions(t), kustomizeDir)
			})

			// Check that we can connect services over the mesh gateways
			logger.Log(t, "creating static-server in dc2")
			k8s.DeployKustomize(t, secondaryContext.KubectlOptions(t), cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/cases/static-server-inject")

			logger.Log(t, "creating static-client in dc1")
			k8s.DeployKustomize(t, primaryContext.KubectlOptions(t), cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/cases/static-client-multi-dc")

			if c.secure {
				logger.Log(t, "creating intention")
				_, _, err = primaryClient.ConfigEntries().Set(&api.ServiceIntentionsConfigEntry{
					Kind: api.ServiceIntentions,
					Name: "static-server",
					Sources: []*api.SourceIntention{
						{
							Name:   StaticClientName,
							Action: api.IntentionActionAllow,
						},
					},
				}, nil)
				require.NoError(t, err)
			}

			logger.Log(t, "checking that connection is successful")
			k8s.CheckStaticServerConnectionSuccessful(t, primaryContext.KubectlOptions(t), StaticClientName, "http://localhost:1234")
		})
	}
}
