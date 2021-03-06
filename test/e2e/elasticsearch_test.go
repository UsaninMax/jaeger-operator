// +build elasticsearch

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/operator-framework/operator-sdk/pkg/test/e2eutil"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/portforward"

	v1 "github.com/jaegertracing/jaeger-operator/pkg/apis/jaegertracing/v1"
)

type ElasticSearchTestSuite struct {
	suite.Suite
}

var esIndexCleanerEnabled = false
var esUrl string
var esNamespace = storageNamespace

func (suite *ElasticSearchTestSuite) SetupSuite() {
	t = suite.T()
	var err error
	ctx, err = prepare(t)
	if err != nil {
		if ctx != nil {
			ctx.Cleanup()
		}
		require.FailNow(t, "Failed in prepare")
	}
	fw = framework.Global
	namespace = ctx.GetID()
	require.NotNil(t, namespace, "GetID failed")

	addToFrameworkSchemeForSmokeTests(t)

	if isOpenShift(t) {
		esServerUrls = "http://elasticsearch." + storageNamespace + ".svc.cluster.local:9200"
	}
}

func (suite *ElasticSearchTestSuite) TearDownSuite() {
	handleSuiteTearDown()
}

func TestElasticSearchSuite(t *testing.T) {
	suite.Run(t, new(ElasticSearchTestSuite))
}

func (suite *ElasticSearchTestSuite) SetupTest() {
	t = suite.T()
}

func (suite *ElasticSearchTestSuite) AfterTest(suiteName, testName string) {
	handleTestFailure()
}

func (suite *ElasticSearchTestSuite) TestSparkDependenciesES() {
	if skipESExternal {
		t.Skip("This test requires an insecure ElasticSearch instance")
	}
	storage := v1.JaegerStorageSpec{
		Type: v1.JaegerESStorage,
		Options: v1.NewOptions(map[string]interface{}{
			"es.server-urls": esServerUrls,
		}),
	}
	err := sparkTest(t, framework.Global, ctx, storage)
	require.NoError(t, err, "SparkTest failed")
}

func (suite *ElasticSearchTestSuite) TestSimpleProd() {
	if skipESExternal {
		t.Skip("This case is covered by the self_provisioned_elasticsearch_test")
	}
	err := WaitForStatefulset(t, fw.KubeClient, storageNamespace, string(v1.JaegerESStorage), retryInterval, timeout)
	require.NoError(t, err, "Error waiting for elasticsearch")

	// create jaeger custom resource
	name := "simple-prod"
	exampleJaeger := getJaegerSimpleProdWithServerUrls(name)
	err = fw.Client.Create(context.TODO(), exampleJaeger, &framework.CleanupOptions{TestContext: ctx, Timeout: timeout, RetryInterval: retryInterval})
	require.NoError(t, err, "Error deploying example Jaeger")
	defer undeployJaegerInstance(exampleJaeger)

	err = e2eutil.WaitForDeployment(t, fw.KubeClient, namespace, name+"-collector", 1, retryInterval, timeout)
	require.NoError(t, err, "Error waiting for collector deployment")

	err = e2eutil.WaitForDeployment(t, fw.KubeClient, namespace, name+"-query", 1, retryInterval, timeout)
	require.NoError(t, err, "Error waiting for query deployment")

	ProductionSmokeTest(name)

	// Make sure we were using the correct collector image
	verifyCollectorImage(name, namespace, specifyOtelImages)
}

func (suite *ElasticSearchTestSuite) TestEsIndexCleanerWithIndexPrefix() {
	esIndexCleanerEnabled = false
	esIndexPrefix := "prefix"
	jaegerInstanceName := "test-es-index-prefixes"
	jaegerInstance := &v1.Jaeger{}

	if skipESExternal {
		esNamespace = namespace
		numberOfDays := 0
		indexCleanerSpec := v1.JaegerEsIndexCleanerSpec{
			Enabled:      &esIndexCleanerEnabled,
			Schedule:     "*/1 * * * *",
			NumberOfDays: &numberOfDays,
		}

		jaegerInstance = getJaegerSelfProvSimpleProd(jaegerInstanceName, namespace, 1)
		jaegerInstance.Spec.Storage.EsIndexCleaner = indexCleanerSpec
		addIndexPrefix(jaegerInstance, esIndexPrefix)

		createESSelfProvDeployment(jaegerInstance, jaegerInstanceName, namespace)
		defer undeployJaegerInstance(jaegerInstance)

		ProductionSmokeTest(jaegerInstanceName)
	} else {
		esNamespace = storageNamespace
		jaegerInstance = getJaegerAllInOne(jaegerInstanceName)
		addIndexPrefix(jaegerInstance, esIndexPrefix)

		err := fw.Client.Create(context.Background(), jaegerInstance, &framework.CleanupOptions{TestContext: ctx, Timeout: timeout, RetryInterval: retryInterval})
		require.NoError(t, err, "Error deploying Jaeger")
		defer undeployJaegerInstance(jaegerInstance)
		err = e2eutil.WaitForDeployment(t, fw.KubeClient, namespace, jaegerInstanceName, 1, retryInterval, timeout)
		require.NoError(t, err, "Error waiting for deployment")

		// Run the smoke test so indices will be created
		AllInOneSmokeTest(jaegerInstanceName)
	}
	// Now verify that we have indices with the prefix we want
	indexWithPrefixExists(esIndexPrefix+"-jaeger-", true, esNamespace)

	// Turn on index clean and make sure we clean up
	turnOnEsIndexCleaner(jaegerInstance)
	indexWithPrefixExists(esIndexPrefix+"-jaeger-", false, esNamespace)

}

func addIndexPrefix(jaegerInstance *v1.Jaeger, esIndexPrefix string) {
	// Add an index prefix to the CR before creating this Jaeger instance
	options := jaegerInstance.Spec.Storage.Options.Map()
	updateOptions := make(map[string]interface{})
	for key, value := range options {
		updateOptions[key] = value
	}
	updateOptions["es.index-prefix"] = esIndexPrefix
	jaegerInstance.Spec.Storage.Options = v1.NewOptions(updateOptions)
}

func (suite *ElasticSearchTestSuite) TestEsIndexCleaner() {
	esIndexCleanerEnabled = false
	jaegerInstanceName := "test-es-index-cleaner"
	jaegerInstance := &v1.Jaeger{}

	if skipESExternal {
		esNamespace = namespace
		numberOfDays := 0
		indexCleanerSpec := v1.JaegerEsIndexCleanerSpec{
			Enabled:      &esIndexCleanerEnabled,
			Schedule:     "*/1 * * * *",
			NumberOfDays: &numberOfDays,
		}

		jaegerInstance = getJaegerSelfProvSimpleProd(jaegerInstanceName, namespace, 1)
		jaegerInstance.Spec.Storage.EsIndexCleaner = indexCleanerSpec
		createESSelfProvDeployment(jaegerInstance, jaegerInstanceName, namespace)
		defer undeployJaegerInstance(jaegerInstance)

		ProductionSmokeTest(jaegerInstanceName)
	} else {
		esNamespace = storageNamespace
		jaegerInstance = getJaegerAllInOne(jaegerInstanceName)

		err := fw.Client.Create(context.Background(), jaegerInstance, &framework.CleanupOptions{TestContext: ctx, Timeout: timeout, RetryInterval: retryInterval})
		require.NoError(t, err, "Error deploying Jaeger")
		defer undeployJaegerInstance(jaegerInstance)

		err = e2eutil.WaitForDeployment(t, fw.KubeClient, namespace, jaegerInstanceName, 1, retryInterval, timeout)
		require.NoError(t, err, "Error waiting for deployment")

		// create span, then make sure indices have been created
		AllInOneSmokeTest(jaegerInstanceName)
	}
	indexWithPrefixExists("jaeger-", true, esNamespace)

	// Once we've created a span with the smoke test, enable the index cleaner
	turnOnEsIndexCleaner(jaegerInstance)

	// Now make sure indices have been deleted
	indexWithPrefixExists("jaeger-", false, esNamespace)
}

func getJaegerSimpleProdWithServerUrls(name string) *v1.Jaeger {
	ingressEnabled := true
	exampleJaeger := &v1.Jaeger{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Jaeger",
			APIVersion: "jaegertracing.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.JaegerSpec{
			Ingress: v1.JaegerIngressSpec{
				Enabled:  &ingressEnabled,
				Security: v1.IngressSecurityNoneExplicit,
			},
			Strategy: v1.DeploymentStrategyProduction,
			Storage: v1.JaegerStorageSpec{
				Type: v1.JaegerESStorage,
				Options: v1.NewOptions(map[string]interface{}{
					"es.server-urls": esServerUrls,
				}),
			},
		},
	}

	if specifyOtelImages {
		logrus.Infof("Using OTEL collector for %s", name)
		exampleJaeger.Spec.Collector.Image = otelCollectorImage
		exampleJaeger.Spec.Collector.Config = v1.NewFreeForm(getOtelConfigForHealthCheckPort("14269"))
	}

	return exampleJaeger
}

func getJaegerAllInOne(name string) *v1.Jaeger {
	numberOfDays := 0
	ingressEnabled := true
	j := &v1.Jaeger{
		TypeMeta: v12.TypeMeta{
			Kind:       "Jaeger",
			APIVersion: "jaegertracing.io/v1",
		},
		ObjectMeta: v12.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.JaegerSpec{
			Ingress: v1.JaegerIngressSpec{
				Enabled:  &ingressEnabled,
				Security: v1.IngressSecurityNoneExplicit,
			},
			Strategy: v1.DeploymentStrategyAllInOne,
			Storage: v1.JaegerStorageSpec{
				Type: v1.JaegerESStorage,
				Options: v1.NewOptions(map[string]interface{}{
					"es.server-urls": esServerUrls,
				}),
				EsIndexCleaner: v1.JaegerEsIndexCleanerSpec{
					Enabled:      &esIndexCleanerEnabled,
					Schedule:     "*/1 * * * *",
					NumberOfDays: &numberOfDays,
				},
			},
		},
	}
	return j
}

func hasIndexWithPrefix(prefix string, esPort string) (bool, error) {
	transport := &http.Transport{}
	if skipESExternal {
		esUrl = "https://localhost:" + esPort + "/_cat/indices"
		esSecret, err := fw.KubeClient.CoreV1().Secrets(namespace).Get(context.Background(), "elasticsearch", metav1.GetOptions{})
		require.NoError(t, err)
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(esSecret.Data["admin-ca"])

		clientCert, err := tls.X509KeyPair(esSecret.Data["admin-cert"], esSecret.Data["admin-key"])
		require.NoError(t, err)

		transport.TLSClientConfig = &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{clientCert},
		}
	} else {
		esUrl = "http://localhost:" + esPort + "/_cat/indices"
	}
	client := http.Client{Transport: transport}

	req, err := http.NewRequest(http.MethodGet, esUrl, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	bodyString := string(bodyBytes)

	return strings.Contains(bodyString, prefix), nil
}

func createEsPortForward(esNamespace string) (portForwES *portforward.PortForwarder, closeChanES chan struct{}, esPort string) {
	portForwES, closeChanES = CreatePortForward(esNamespace, string(v1.JaegerESStorage), string(v1.JaegerESStorage), []string{"0:9200"}, fw.KubeConfig)
	forwardedPorts, err := portForwES.GetPorts()
	require.NoError(t, err)
	return portForwES, closeChanES, strconv.Itoa(int(forwardedPorts[0].Local))
}

func turnOnEsIndexCleaner(jaegerInstance *v1.Jaeger) {
	key := types.NamespacedName{Name: jaegerInstance.Name, Namespace: jaegerInstance.GetNamespace()}
	err := fw.Client.Get(context.Background(), key, jaegerInstance)
	require.NoError(t, err)
	esIndexCleanerEnabled = true
	err = fw.Client.Update(context.Background(), jaegerInstance)
	require.NoError(t, err)

	err = WaitForCronJob(t, fw.KubeClient, namespace, fmt.Sprintf("%s-es-index-cleaner", jaegerInstance.Name), retryInterval, timeout+1*time.Minute)
	require.NoError(t, err, "Error waiting for Cron Job")

	err = WaitForJobOfAnOwner(t, fw.KubeClient, namespace, fmt.Sprintf("%s-es-index-cleaner", jaegerInstance.Name), retryInterval, timeout)
	require.NoError(t, err, "Error waiting for Cron Job")
}

func indexWithPrefixExists(prefix string, condition bool, esNamespace string) {
	portForwES, closeChanES, esPort := createEsPortForward(esNamespace)
	defer portForwES.Close()
	defer close(closeChanES)
	err := wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		flag, err := hasIndexWithPrefix(prefix, esPort)
		return flag == condition, err
	})
	require.NoError(t, err)
}
