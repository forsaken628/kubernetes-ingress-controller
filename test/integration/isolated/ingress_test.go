//go:build integration_tests

package isolated

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/kong/kubernetes-testing-framework/pkg/clusters"
	ktfkong "github.com/kong/kubernetes-testing-framework/pkg/clusters/addons/kong"
	"github.com/kong/kubernetes-testing-framework/pkg/utils/kubernetes/generators"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/kong/kubernetes-ingress-controller/v3/internal/annotations"
	"github.com/kong/kubernetes-ingress-controller/v3/internal/util/builder"
	"github.com/kong/kubernetes-ingress-controller/v3/test"
	"github.com/kong/kubernetes-ingress-controller/v3/test/helpers/certificate"
	"github.com/kong/kubernetes-ingress-controller/v3/test/integration/consts"
	"github.com/kong/kubernetes-ingress-controller/v3/test/internal/testlabels"
)

func TestIngressGRPC(t *testing.T) {
	const testHostname = "grpcs-over-ingress.example"

	f := features.
		New("essentials").
		WithLabel(testlabels.NetworkingFamily, testlabels.NetworkingFamilyIngress).
		WithLabel(testlabels.Kind, testlabels.KindIngress).
		WithSetup("deploy kong addon into cluster", featureSetup(
			withKongProxyEnvVars(map[string]string{
				"PROXY_LISTEN": `0.0.0.0:8000 http2\, 0.0.0.0:8443 http2 ssl`,
			}),
		)).
		WithSetup("deploying gRPC service exposed via Ingress", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cleaner := GetFromCtxForT[*clusters.Cleaner](ctx, t)
			cluster := GetClusterFromCtx(ctx)
			namespace := GetNamespaceForT(ctx, t)

			t.Log("configuring secret")
			tlsRouteExampleTLSCert, tlsRouteExampleTLSKey := certificate.MustGenerateSelfSignedCertPEMFormat(certificate.WithCommonName(testHostname))
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: "secret-test",
				},
				Data: map[string][]byte{
					"tls.crt": tlsRouteExampleTLSCert,
					"tls.key": tlsRouteExampleTLSKey,
				},
			}

			t.Log("deploying secret")
			secret, err := cluster.Client().CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
			assert.NoError(t, err)
			cleaner.Add(secret)

			type kongProtocolAnnotation string
			const (
				gRPC  kongProtocolAnnotation = "grpc"
				gRPCS kongProtocolAnnotation = "grpcs"
			)
			const (
				gRPCBinPort  int32 = 9000
				gRPCSBinPort int32 = 9001
			)
			t.Log("deploying a minimal gRPC container deployment to test Ingress routes")
			container := generators.NewContainer("grpcbin", test.GRPCBinImage, 0)
			// Overwrite ports to specify gRPC over HTTP (9000) and gRPC over HTTPS (9001).
			container.Ports = []corev1.ContainerPort{{ContainerPort: gRPCBinPort, Name: string(gRPC)}, {ContainerPort: gRPCSBinPort, Name: string(gRPCS)}}
			deployment := generators.NewDeploymentForContainer(container)
			deployment, err = cluster.Client().AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
			assert.NoError(t, err)
			cleaner.Add(deployment)

			exposeWithService := func(p kongProtocolAnnotation) *corev1.Service {
				grpcBinPort := gRPCBinPort
				if p == gRPCS {
					grpcBinPort = gRPCSBinPort
				}
				kongProtocol := string(p)
				t.Logf("exposing deployment gRPC (%s) port %s via service", kongProtocol, deployment.Name)
				svc := generators.NewServiceForDeploymentWithMappedPorts(deployment, corev1.ServiceTypeLoadBalancer, map[int32]int32{grpcBinPort: grpcBinPort})
				svc.Name += kongProtocol
				svc.Annotations = map[string]string{
					annotations.AnnotationPrefix + annotations.ProtocolKey: kongProtocol,
				}
				_, err = cluster.Client().CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
				assert.NoError(t, err)
				cleaner.Add(svc)
				return svc
			}

			// Deploy two services, one for gRPC and one for gRPCS. Two protocols in one service annotation (konghq.com/protocol) are not supported.
			serviceGRPC := exposeWithService(gRPC)
			serviceGRPCS := exposeWithService(gRPCS)

			ingressClass := GetIngressClassFromCtx(ctx)
			t.Logf("creating an ingress for services: %s, %s with ingress.class %s", serviceGRPC.Name, serviceGRPCS.Name, ingressClass)
			ingress := builder.NewIngress(uuid.NewString(), ingressClass).WithRules(
				netv1.IngressRule{
					Host: testHostname,
					IngressRuleValue: netv1.IngressRuleValue{
						HTTP: &netv1.HTTPIngressRuleValue{
							Paths: []netv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: lo.ToPtr(netv1.PathTypePrefix),
									Backend: netv1.IngressBackend{
										Service: &netv1.IngressServiceBackend{
											Name: serviceGRPCS.Name,
											Port: netv1.ServiceBackendPort{
												Number: gRPCSBinPort,
											},
										},
									},
								},
							},
						},
					},
				},
				netv1.IngressRule{
					IngressRuleValue: netv1.IngressRuleValue{
						HTTP: &netv1.HTTPIngressRuleValue{
							Paths: []netv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: lo.ToPtr(netv1.PathTypePrefix),
									Backend: netv1.IngressBackend{
										Service: &netv1.IngressServiceBackend{
											Name: serviceGRPC.Name,
											Port: netv1.ServiceBackendPort{
												Number: gRPCBinPort,
											},
										},
									},
								},
							},
						},
					},
				},
			).Build()
			ingress.Annotations[annotations.AnnotationPrefix+annotations.ProtocolsKey] = fmt.Sprintf("%s,%s", gRPC, gRPCS)
			assert.NoError(t, clusters.DeployIngress(ctx, cluster, namespace, ingress))
			cleaner.Add(ingress)
			ctx = SetInCtxForT(ctx, t, ingress)

			return ctx
		}).
		Assess("checking whether Ingress status is updated and gRPC traffic over HTTPS is properly routed", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			t.Log("waiting for updated ingress status to include IP")
			assert.Eventually(t, func() bool {
				cluster := GetClusterFromCtx(ctx)
				namespace := GetNamespaceForT(ctx, t)
				ingress := GetFromCtxForT[*netv1.Ingress](ctx, t)

				lbstatus, err := clusters.GetIngressLoadbalancerStatus(ctx, cluster, namespace, ingress)
				if err != nil {
					return false
				}
				return len(lbstatus.Ingress) > 0
			}, consts.StatusWait, consts.WaitTick)

			verifyEchoResponds := func(hostname string) {
				// Kong Gateway uses different ports for HTTP and HTTPS traffic.
				proxyPort := ktfkong.DefaultProxyTLSServicePort
				tlsEnabled := true
				if hostname == "" {
					proxyPort = ktfkong.DefaultProxyHTTPPort
					tlsEnabled = false
				}
				assert.Eventually(t, func() bool {
					if err := grpcEchoResponds(
						ctx, fmt.Sprintf("%s:%d", GetProxyURLFromCtx(ctx).Hostname(), proxyPort), hostname, "echo Kong", tlsEnabled,
					); err != nil {
						t.Log(err)
						return false
					}
					return true
				}, consts.IngressWait, consts.WaitTick)
			}
			t.Log("verifying service connectivity via HTTPS (gRPCS)")
			verifyEchoResponds(testHostname)
			t.Log("verifying service connectivity via HTTP (gRPC)")
			verifyEchoResponds("")

			return ctx
		}).
		Teardown(featureTeardown())

	tenv.Test(t, f.Feature())
}