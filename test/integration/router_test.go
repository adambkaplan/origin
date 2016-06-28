// +build integration,docker

package integration

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	dockerClient "github.com/fsouza/go-dockerclient"
	"golang.org/x/net/websocket"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/api/v1beta3"
	"k8s.io/kubernetes/pkg/util/intstr"
	knet "k8s.io/kubernetes/pkg/util/net"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/watch"
	watchjson "k8s.io/kubernetes/pkg/watch/json"

	routeapi "github.com/openshift/origin/pkg/route/api"
	tr "github.com/openshift/origin/test/integration/router"
	testutil "github.com/openshift/origin/test/util"
)

const (
	defaultRouterImage = "openshift/origin-haproxy-router"

	tcWaitSeconds = 1

	statsPort     = 1936
	statsUser     = "admin"
	statsPassword = "e2e"
)

// TestRouter is the table based test for routers.  It will initialize a fake master/client and expect to deploy
// a router image in docker.  It then sends watch events through the simulator and makes http client requests that
// should go through the deployed router and return data from the client simulator.
func TestRouter(t *testing.T) {
	//create a server which will act as a user deployed application that
	//serves http and https as well as act as a master to simulate watches
	fakeMasterAndPod := tr.NewTestHttpService()
	defer fakeMasterAndPod.Stop()

	err := fakeMasterAndPod.Start()
	validateServer(fakeMasterAndPod, t)

	if err != nil {
		t.Fatalf("Unable to start http server: %v", err)
	}

	//deploy router docker container
	dockerCli, err := testutil.NewDockerClient()

	if err != nil {
		t.Fatalf("Unable to get docker client: %v", err)
	}

	routerId, err := createAndStartRouterContainer(dockerCli, fakeMasterAndPod.MasterHttpAddr, statsPort, 1)

	if err != nil {
		t.Fatalf("Error starting container %s : %v", getRouterImage(), err)
	}

	defer cleanUp(t, dockerCli, routerId)

	httpEndpoint, err := getEndpoint(fakeMasterAndPod.PodHttpAddr)
	if err != nil {
		t.Fatalf("Couldn't get http endpoint: %v", err)
	}
	httpsEndpoint, err := getEndpoint(fakeMasterAndPod.PodHttpsAddr)
	if err != nil {
		t.Fatalf("Couldn't get https endpoint: %v", err)
	}
	alternateHttpEndpoint, err := getEndpoint(fakeMasterAndPod.AlternatePodHttpAddr)
	if err != nil {
		t.Fatalf("Couldn't get http endpoint: %v", err)
	}

	routeAddress := getRouteAddress()
	routeTestAddress := fmt.Sprintf("%s/test", routeAddress)
	routerEchoHttpAddress := fmt.Sprintf("%s:80/echo", routeAddress)
	routerEchoHttpsAddress := fmt.Sprintf("%s:443/echo", routeAddress)

	//run through test cases now that environment is set up
	testCases := []struct {
		name              string
		serviceName       string
		endpoints         []kapi.EndpointSubset
		routeAlias        string
		routePath         string
		endpointEventType watch.EventType
		routeEventType    watch.EventType
		protocol          string
		expectedResponse  string
		routeTLS          *routeapi.TLSConfig
		routerUrl         string
		preferredPort     *routeapi.RoutePort
	}{
		{
			name:              "non-secure",
			serviceName:       "example",
			endpoints:         []kapi.EndpointSubset{httpEndpoint},
			routeAlias:        "www.example-unsecure.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "http",
			expectedResponse:  tr.HelloPod,
			routeTLS:          nil,
			routerUrl:         routeAddress,
		},
		{
			name:              "non-secure-path",
			serviceName:       "example-path",
			endpoints:         []kapi.EndpointSubset{httpEndpoint},
			routeAlias:        "www.example-unsecure.com",
			routePath:         "/test",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "http",
			expectedResponse:  tr.HelloPodPath,
			routeTLS:          nil,
			routerUrl:         routeTestAddress,
		},
		{
			name:              "preferred-port",
			serviceName:       "example-preferred-port",
			endpoints:         []kapi.EndpointSubset{alternateHttpEndpoint, httpEndpoint},
			routeAlias:        "www.example-unsecure.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "http",
			expectedResponse:  tr.HelloPod,
			routeTLS:          nil,
			routerUrl:         routeAddress,
			preferredPort:     &routeapi.RoutePort{TargetPort: intstr.FromInt(8888)},
		},
		{
			name:              "edge termination",
			serviceName:       "example-edge",
			endpoints:         []kapi.EndpointSubset{httpEndpoint},
			routeAlias:        "www.example.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "https",
			expectedResponse:  tr.HelloPod,
			routeTLS: &routeapi.TLSConfig{
				Termination:   routeapi.TLSTerminationEdge,
				Certificate:   tr.ExampleCert,
				Key:           tr.ExampleKey,
				CACertificate: tr.ExampleCACert,
			},
			routerUrl: routeAddress,
		},
		{
			name:              "edge termination path",
			serviceName:       "example-edge-path",
			endpoints:         []kapi.EndpointSubset{httpEndpoint},
			routeAlias:        "www.example.com",
			routePath:         "/test",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "https",
			expectedResponse:  tr.HelloPodPath,
			routeTLS: &routeapi.TLSConfig{
				Termination:   routeapi.TLSTerminationEdge,
				Certificate:   tr.ExampleCert,
				Key:           tr.ExampleKey,
				CACertificate: tr.ExampleCACert,
			},
			routerUrl: routeTestAddress,
		},
		{
			name:              "reencrypt",
			serviceName:       "example-reencrypt",
			endpoints:         []kapi.EndpointSubset{httpsEndpoint},
			routeAlias:        "www.example.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "https",
			expectedResponse:  tr.HelloPodSecure,
			routeTLS: &routeapi.TLSConfig{
				Termination:              routeapi.TLSTerminationReencrypt,
				Certificate:              tr.ExampleCert,
				Key:                      tr.ExampleKey,
				CACertificate:            tr.ExampleCACert,
				DestinationCACertificate: tr.ExampleCACert,
			},
			routerUrl: "0.0.0.0",
		},
		{
			name:              "reencrypt-destcacert",
			serviceName:       "example-reencrypt-destcacert",
			endpoints:         []kapi.EndpointSubset{httpsEndpoint},
			routeAlias:        "www.example.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "https",
			expectedResponse:  tr.HelloPodSecure,
			routeTLS: &routeapi.TLSConfig{
				Termination:              routeapi.TLSTerminationReencrypt,
				DestinationCACertificate: tr.ExampleCACert,
			},
			routerUrl: "0.0.0.0",
		},
		{
			name:              "reencrypt path",
			serviceName:       "example-reencrypt-path",
			endpoints:         []kapi.EndpointSubset{httpsEndpoint},
			routeAlias:        "www.example.com",
			routePath:         "/test",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "https",
			expectedResponse:  tr.HelloPodPathSecure,
			routeTLS: &routeapi.TLSConfig{
				Termination:              routeapi.TLSTerminationReencrypt,
				Certificate:              tr.ExampleCert,
				Key:                      tr.ExampleKey,
				CACertificate:            tr.ExampleCACert,
				DestinationCACertificate: tr.ExampleCACert,
			},
			routerUrl: "0.0.0.0/test",
		},
		{
			name:              "passthrough termination",
			serviceName:       "example-passthrough",
			endpoints:         []kapi.EndpointSubset{httpsEndpoint},
			routeAlias:        "www.example-passthrough.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "https",
			expectedResponse:  tr.HelloPodSecure,
			routeTLS: &routeapi.TLSConfig{
				Termination: routeapi.TLSTerminationPassthrough,
			},
			routerUrl: routeAddress,
		},
		{
			name:              "websocket unsecure",
			serviceName:       "websocket-unsecure",
			endpoints:         []kapi.EndpointSubset{httpEndpoint},
			routeAlias:        "www.example.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "ws",
			expectedResponse:  "hello-websocket-unsecure",
			routerUrl:         routerEchoHttpAddress,
		},
		{
			name:              "ws edge termination",
			serviceName:       "websocket-edge",
			endpoints:         []kapi.EndpointSubset{httpEndpoint},
			routeAlias:        "www.example.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "wss",
			expectedResponse:  "hello-websocket-edge",
			routeTLS: &routeapi.TLSConfig{
				Termination:   routeapi.TLSTerminationEdge,
				Certificate:   tr.ExampleCert,
				Key:           tr.ExampleKey,
				CACertificate: tr.ExampleCACert,
			},
			routerUrl: routerEchoHttpsAddress,
		},
		{
			name:              "ws passthrough termination",
			serviceName:       "websocket-passthrough",
			endpoints:         []kapi.EndpointSubset{httpsEndpoint},
			routeAlias:        "www.example.com",
			endpointEventType: watch.Added,
			routeEventType:    watch.Added,
			protocol:          "wss",
			expectedResponse:  "hello-websocket-passthrough",
			routeTLS: &routeapi.TLSConfig{
				Termination: routeapi.TLSTerminationPassthrough,
			},
			routerUrl: routerEchoHttpsAddress,
		},
	}

	ns := "rotorouter"
	for _, tc := range testCases {
		// The following is a workaround for the websocket client, which does not
		// allow a "Host" header that is distinct from the address to which the
		// client code attempts to connect—so if we are putting "www.example.com" in
		// the "Host" header, the client will connect to "www.example.com".
		//
		// In the case where we use HAProxy (with the template router), it is
		// possible to use 0.0.0.0, so we can do so as a workaround to get the tests
		// passing with the template router.  In the case of the F5 router though,
		// F5 BIG-IP would reject 0.0.0.0 as an invalid servername, so the only way
		// to make the tests pass with the F5 router is to use a hostname and make
		// that hostname resolve to the F5 BIG-IP host's IP address.
		if getRouterImage() == defaultRouterImage &&
			(tc.protocol == "ws" || tc.protocol == "wss") {
			tc.routeAlias = "0.0.0.0"
		}

		// Simulate the events.
		endpointEvent := &watch.Event{
			Type: tc.endpointEventType,

			Object: &kapi.Endpoints{
				ObjectMeta: kapi.ObjectMeta{
					Name:      tc.serviceName,
					Namespace: ns,
				},
				Subsets: tc.endpoints,
			},
		}

		routeEvent := &watch.Event{
			Type: tc.routeEventType,
			Object: &routeapi.Route{
				ObjectMeta: kapi.ObjectMeta{
					Name:      tc.serviceName,
					Namespace: ns,
				},
				Spec: routeapi.RouteSpec{
					Host: tc.routeAlias,
					Path: tc.routePath,
					To: kapi.ObjectReference{
						Name: tc.serviceName,
					},
					TLS: tc.routeTLS,
				},
			},
		}
		if tc.preferredPort != nil {
			routeEvent.Object.(*routeapi.Route).Spec.Port = tc.preferredPort
		}

		fakeMasterAndPod.EndpointChannel <- eventString(endpointEvent)
		fakeMasterAndPod.RouteChannel <- eventString(routeEvent)

		// Now verify the route with an HTTP client.
		if err := waitForRoute(tc.routerUrl, tc.routeAlias, tc.protocol, nil, tc.expectedResponse); err != nil {
			t.Errorf("TC %s failed: %v", tc.name, err)

			// The following is related to the workaround above, q.v.
			if getRouterImage() != defaultRouterImage {
				t.Errorf("You may need to add an entry to /etc/hosts so that the"+
					" hostname of the router (%s) resolves its the IP address, (%s).",
					tc.routeAlias, routeAddress)
			}
		}

		//clean up
		routeEvent.Type = watch.Deleted
		endpointEvent.Type = watch.Modified
		endpoints := endpointEvent.Object.(*kapi.Endpoints)
		endpoints.Subsets = []kapi.EndpointSubset{}

		fakeMasterAndPod.EndpointChannel <- eventString(endpointEvent)
		fakeMasterAndPod.RouteChannel <- eventString(routeEvent)
	}
}

// TestRouterPathSpecificity tests that the router is matching routes from most specific to least when using
// a combination of path AND host based routes.  It also ensures that a host based route still allows path based
// matches via the host header.
//
// For example, the http server simulator acts as if it has a directory structure like:
// /var/www
//         index.html (Hello Pod)
//         /test
//              index.html (Hello Pod Path)
//
// With just a path based route for www.example.com/test I should get Hello Pod Path for a curl to www.example.com/test
// A curl to www.example.com should fall through to the default handlers.  In the test environment it will fall through
// to a call to 0.0.0.0:8080 which is the master simulator
//
// If a host based route for www.example.com is added into the mix I should then be able to curl www.example.com and get
// Hello Pod and still be able to curl www.example.com/test and get Hello Pod Path
//
// If the path based route is deleted I should still be able to curl both routes successfully using the host based path
func TestRouterPathSpecificity(t *testing.T) {
	fakeMasterAndPod := tr.NewTestHttpService()
	err := fakeMasterAndPod.Start()
	if err != nil {
		t.Fatalf("Unable to start http server: %v", err)
	}
	defer fakeMasterAndPod.Stop()

	validateServer(fakeMasterAndPod, t)

	dockerCli, err := testutil.NewDockerClient()
	if err != nil {
		t.Fatalf("Unable to get docker client: %v", err)
	}

	routerId, err := createAndStartRouterContainer(dockerCli, fakeMasterAndPod.MasterHttpAddr, statsPort, 1)
	if err != nil {
		t.Fatalf("Error starting container %s : %v", getRouterImage(), err)
	}
	defer cleanUp(t, dockerCli, routerId)

	httpEndpoint, err := getEndpoint(fakeMasterAndPod.PodHttpAddr)
	if err != nil {
		t.Fatalf("Couldn't get http endpoint: %v", err)
	}

	alternateHttpEndpoint, err := getEndpoint(fakeMasterAndPod.AlternatePodHttpAddr)
	if err != nil {
		t.Fatalf("Couldn't get http endpoint: %v", err)
	}

	waitForRouterToBecomeAvailable("127.0.0.1", statsPort)

	now := unversioned.Now()

	//create path based route
	endpointEvent := &watch.Event{
		Type: watch.Added,
		Object: &kapi.Endpoints{
			ObjectMeta: kapi.ObjectMeta{
				CreationTimestamp: now,
				Name:              "myService",
				Namespace:         "default",
			},
			Subsets: []kapi.EndpointSubset{httpEndpoint},
		},
	}
	routeEvent := &watch.Event{
		Type: watch.Added,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "path",
				Namespace: "default",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				Path: "/test",
				To: kapi.ObjectReference{
					Name: "myService",
				},
			},
		},
	}

	routeAddress := getRouteAddress()
	routeTestAddress := fmt.Sprintf("%s/test", routeAddress)

	fakeMasterAndPod.EndpointChannel <- eventString(endpointEvent)
	fakeMasterAndPod.RouteChannel <- eventString(routeEvent)

	//ensure you can curl path but not main host
	if err := waitForRoute(routeTestAddress, "www.example.com", "http", nil, tr.HelloPodPath); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}
	if _, err := getRoute(routeAddress, "www.example.com", "http", nil, ""); err != ErrUnavailable {
		t.Fatalf("unexpected response: %q", err)
	}

	//create newer, conflicting path based route
	endpointEvent = &watch.Event{
		Type: watch.Added,
		Object: &kapi.Endpoints{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "altService",
				Namespace: "alt",
			},
			Subsets: []kapi.EndpointSubset{alternateHttpEndpoint},
		},
	}
	routeEvent = &watch.Event{
		Type: watch.Added,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				CreationTimestamp: unversioned.Time{Time: now.Add(time.Hour)},
				Name:              "path",
				Namespace:         "alt",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				Path: "/test",
				To: kapi.ObjectReference{
					Name: "altService",
				},
			},
		},
	}
	fakeMasterAndPod.EndpointChannel <- eventString(endpointEvent)
	fakeMasterAndPod.RouteChannel <- eventString(routeEvent)

	if err := waitForRoute(routeTestAddress, "www.example.com", "http", nil, tr.HelloPodPath); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}

	//create host based route
	routeEvent = &watch.Event{
		Type: watch.Added,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				CreationTimestamp: now,
				Name:              "host",
				Namespace:         "default",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				To: kapi.ObjectReference{
					Name: "myService",
				},
			},
		},
	}
	fakeMasterAndPod.RouteChannel <- eventString(routeEvent)

	//ensure you can curl path and host
	if err := waitForRoute(routeTestAddress, "www.example.com", "http", nil, tr.HelloPodPath); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}
	if err := waitForRoute(routeAddress, "www.example.com", "http", nil, tr.HelloPod); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}

	//delete path based route
	routeEvent = &watch.Event{
		Type: watch.Deleted,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "path",
				Namespace: "default",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				Path: "/test",
				To: kapi.ObjectReference{
					Name: "myService",
				},
			},
		},
	}
	fakeMasterAndPod.RouteChannel <- eventString(routeEvent)

	// Ensure you can still curl path and host.  The host-based route should now
	// handle requests to / as well as requests to /test (or any other path).
	// Note, however, that the host-based route and the host-based route use the
	// same service, and that that service varies its response in accordance with
	// the path, so we still get the tr.HelloPodPath response when we request
	// /test even though we request using routeAddress.
	if err := waitForRoute(routeTestAddress, "www.example.com", "http", nil, tr.HelloPodPath); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}
	if err := waitForRoute(routeAddress, "www.example.com", "http", nil, tr.HelloPod); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}

	// create newer, conflicting host based route that is ignored
	routeEvent = &watch.Event{
		Type: watch.Added,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				CreationTimestamp: unversioned.Time{Time: now.Add(time.Hour)},
				Name:              "host",
				Namespace:         "alt",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				To: kapi.ObjectReference{
					Name: "altService",
				},
			},
		},
	}
	fakeMasterAndPod.RouteChannel <- eventString(routeEvent)

	if err := waitForRoute(routeTestAddress, "www.example.com", "http", nil, tr.HelloPodPath); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}
	if err := waitForRoute(routeAddress, "www.example.com", "http", nil, tr.HelloPod); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}

	//create old, conflicting host based route which should take over the route
	routeEvent = &watch.Event{
		Type: watch.Added,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				CreationTimestamp: unversioned.Time{Time: now.Add(-time.Hour)},
				Name:              "host",
				Namespace:         "alt",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				To: kapi.ObjectReference{
					Name: "altService",
				},
			},
		},
	}
	fakeMasterAndPod.RouteChannel <- eventString(routeEvent)

	if err := waitForRoute(routeTestAddress, "www.example.com", "http", nil, tr.HelloPodAlternate); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}
	if err := waitForRoute(routeAddress, "www.example.com", "http", nil, tr.HelloPodAlternate); err != nil {
		t.Fatalf("unexpected response: %q", err)
	}

	// Clean up the host-based route and endpoint.
	routeEvent = &watch.Event{
		Type: watch.Deleted,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "host",
				Namespace: "default",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				To: kapi.ObjectReference{
					Name: "myService",
				},
			},
		},
	}
	fakeMasterAndPod.RouteChannel <- eventString(routeEvent)
	endpointEvent = &watch.Event{
		Type: watch.Modified,
		Object: &kapi.Endpoints{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "myService",
				Namespace: "default",
			},
			Subsets: []kapi.EndpointSubset{},
		},
	}
	fakeMasterAndPod.EndpointChannel <- eventString(endpointEvent)
}

// TestRouterDuplications ensures that the router implementation is keying correctly and resolving routes that may be
// using the same services with different hosts
func TestRouterDuplications(t *testing.T) {
	fakeMasterAndPod := tr.NewTestHttpService()
	err := fakeMasterAndPod.Start()
	if err != nil {
		t.Fatalf("Unable to start http server: %v", err)
	}
	defer fakeMasterAndPod.Stop()

	validateServer(fakeMasterAndPod, t)

	dockerCli, err := testutil.NewDockerClient()
	if err != nil {
		t.Fatalf("Unable to get docker client: %v", err)
	}

	routerId, err := createAndStartRouterContainer(dockerCli, fakeMasterAndPod.MasterHttpAddr, statsPort, 1)
	if err != nil {
		t.Fatalf("Error starting container %s : %v", getRouterImage(), err)
	}
	defer cleanUp(t, dockerCli, routerId)

	httpEndpoint, err := getEndpoint(fakeMasterAndPod.PodHttpAddr)
	if err != nil {
		t.Fatalf("Couldn't get http endpoint: %v", err)
	}

	waitForRouterToBecomeAvailable("127.0.0.1", statsPort)

	//create routes
	endpointEvent := &watch.Event{
		Type: watch.Added,
		Object: &kapi.Endpoints{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "myService",
				Namespace: "default",
			},
			Subsets: []kapi.EndpointSubset{httpEndpoint},
		},
	}
	exampleRouteEvent := &watch.Event{
		Type: watch.Added,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "example",
				Namespace: "default",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				To: kapi.ObjectReference{
					Name: "myService",
				},
			},
		},
	}
	example2RouteEvent := &watch.Event{
		Type: watch.Added,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "example2",
				Namespace: "default",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example2.com",
				To: kapi.ObjectReference{
					Name: "myService",
				},
			},
		},
	}

	fakeMasterAndPod.EndpointChannel <- eventString(endpointEvent)
	fakeMasterAndPod.RouteChannel <- eventString(exampleRouteEvent)
	fakeMasterAndPod.RouteChannel <- eventString(example2RouteEvent)

	routeAddress := getRouteAddress()

	//ensure you can curl both
	err1 := waitForRoute(routeAddress, "www.example.com", "http", nil, tr.HelloPod)
	err2 := waitForRoute(routeAddress, "www.example2.com", "http", nil, tr.HelloPod)

	if err1 != nil || err2 != nil {
		t.Errorf("Unable to validate both routes in a duplicate service scenario.  Resp 1: %s, Resp 2: %s", err1, err2)
	}

	// Clean up the endpoint and routes.
	example2RouteCleanupEvent := &watch.Event{
		Type: watch.Deleted,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "example2",
				Namespace: "default",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example2.com",
				To: kapi.ObjectReference{
					Name: "myService",
				},
			},
		},
	}
	fakeMasterAndPod.RouteChannel <- eventString(example2RouteCleanupEvent)
	exampleRouteCleanupEvent := &watch.Event{
		Type: watch.Deleted,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "example",
				Namespace: "default",
			},
			Spec: routeapi.RouteSpec{
				Host: "www.example.com",
				To: kapi.ObjectReference{
					Name: "myService",
				},
			},
		},
	}
	fakeMasterAndPod.RouteChannel <- eventString(exampleRouteCleanupEvent)
	endpointCleanupEvent := &watch.Event{
		Type: watch.Modified,
		Object: &kapi.Endpoints{
			ObjectMeta: kapi.ObjectMeta{
				Name:      "myService",
				Namespace: "default",
			},
			Subsets: []kapi.EndpointSubset{},
		},
	}
	fakeMasterAndPod.EndpointChannel <- eventString(endpointCleanupEvent)
}

// TestRouterStatsPort tests that the router is listening on and
// exposing statistics for the default haproxy router image.
func TestRouterStatsPort(t *testing.T) {
	fakeMasterAndPod := tr.NewTestHttpService()
	err := fakeMasterAndPod.Start()
	if err != nil {
		t.Fatalf("Unable to start http server: %v", err)
	}
	defer fakeMasterAndPod.Stop()

	validateServer(fakeMasterAndPod, t)

	dockerCli, err := testutil.NewDockerClient()
	if err != nil {
		t.Fatalf("Unable to get docker client: %v", err)
	}

	routerId, err := createAndStartRouterContainer(dockerCli, fakeMasterAndPod.MasterHttpAddr, statsPort, 1)
	if err != nil {
		t.Fatalf("Error starting container %s : %v", getRouterImage(), err)
	}
	defer cleanUp(t, dockerCli, routerId)

	waitForRouterToBecomeAvailable("127.0.0.1", statsPort)

	statsHostPort := fmt.Sprintf("%s:%d", "127.0.0.1", statsPort)
	creds := fmt.Sprintf("%s:%s", statsUser, statsPassword)
	auth := fmt.Sprintf("Basic: %s", base64.StdEncoding.EncodeToString([]byte(creds)))
	headers := map[string]string{"Authorization": auth}

	if err := waitForRoute(statsHostPort, statsHostPort, "http", headers, ""); err != ErrUnauthenticated {
		t.Fatalf("Unable to verify response: %v", err)
	}
}

// TestRouterHealthzEndpoint tests that the router is listening on and
// exposing the /healthz endpoint for the default haproxy router image.
func TestRouterHealthzEndpoint(t *testing.T) {
	testCases := []struct {
		name string
		port int
	}{
		{
			name: "stats port enabled",
			port: statsPort,
		},
		{
			name: "stats port disabled",
			port: 0,
		},
		{
			name: "custom stats port",
			port: 6391,
		},
	}

	fakeMasterAndPod := tr.NewTestHttpService()
	err := fakeMasterAndPod.Start()
	if err != nil {
		t.Fatalf("Unable to start http server: %v", err)
	}
	defer fakeMasterAndPod.Stop()

	validateServer(fakeMasterAndPod, t)

	dockerCli, err := testutil.NewDockerClient()
	if err != nil {
		t.Fatalf("Unable to get docker client: %v", err)
	}

	for _, tc := range testCases {
		routerId, err := createAndStartRouterContainer(dockerCli, fakeMasterAndPod.MasterHttpAddr, tc.port, 1)
		if err != nil {
			t.Fatalf("Test with %q error starting container %s : %v", tc.name, getRouterImage(), err)
		}
		defer cleanUp(t, dockerCli, routerId)

		host := "127.0.0.1"
		port := tc.port
		if tc.port == 0 {
			port = statsPort
		}

		hostAndPort := fmt.Sprintf("%s:%d", host, port)
		uri := fmt.Sprintf("%s/healthz", hostAndPort)
		if err := waitForRoute(uri, hostAndPort, "http", nil, ""); err != nil {
			t.Errorf("Test with %q unable to verify response: %v", tc.name, err)
		}
	}
}

// TestRouterServiceUnavailable tests that the router returns valid service
// unavailable error pages with appropriate HTTP headers.`
func TestRouterServiceUnavailable(t *testing.T) {
	fakeMasterAndPod := tr.NewTestHttpService()
	err := fakeMasterAndPod.Start()
	if err != nil {
		t.Fatalf("Unable to start http server: %v", err)
	}
	defer fakeMasterAndPod.Stop()

	validateServer(fakeMasterAndPod, t)

	dockerCli, err := testutil.NewDockerClient()
	if err != nil {
		t.Fatalf("Unable to get docker client: %v", err)
	}

	routerId, err := createAndStartRouterContainer(dockerCli, fakeMasterAndPod.MasterHttpAddr, statsPort, 1)
	if err != nil {
		t.Fatalf("Error starting container %s : %v", getRouterImage(), err)
	}
	defer cleanUp(t, dockerCli, routerId)

	waitForRouterToBecomeAvailable("127.0.0.1", statsPort)

	schemes := []string{"http", "https"}
	for _, scheme := range schemes {
		uri := fmt.Sprintf("%s://%s", scheme, getRouteAddress())
		hostAlias := fmt.Sprintf("www.route-%d.test", time.Now().UnixNano())
		var tlsConfig *tls.Config
		if scheme == "https" {
			tlsConfig = &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         hostAlias,
			}
		}

		httpClient := &http.Client{
			Transport: knet.SetTransportDefaults(&http.Transport{
				TLSClientConfig: tlsConfig,
			}),
		}
		req, err := http.NewRequest("GET", uri, nil)
		if err != nil {
			t.Fatalf("Error creating %s request : %v", scheme, err)
		}

		req.Host = hostAlias
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("Error dispatching %s request : %v", scheme, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 503 {
			t.Fatalf("Router %s response error, got %v expected 503.", scheme, resp.StatusCode)
		}

		headerNames := []string{"Pragma", "Cache-Control"}
		for _, k := range headerNames {
			value := resp.Header.Get(k)
			if len(value) == 0 {
				t.Errorf("Router %s response empty/no header %q",
					scheme, k)
			}

			directive := "no-cache"
			if !strings.Contains(value, directive) {
				t.Errorf("Router %s response header %q missing %s response directive",
					scheme, k, directive)
			}
		}

		respBody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("Unable to verify router %s response: %v",
				scheme, err)
		}
		if len(respBody) < 1 {
			t.Errorf("Router %s response body was empty!", scheme)
		}
	}
}

func getEndpoint(hostport string) (kapi.EndpointSubset, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return kapi.EndpointSubset{}, err
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		return kapi.EndpointSubset{}, err
	}
	return kapi.EndpointSubset{Addresses: []kapi.EndpointAddress{{IP: host}}, Ports: []kapi.EndpointPort{{Port: portNum}}}, nil
}

var (
	ErrUnavailable     = fmt.Errorf("endpoint not available")
	ErrUnauthenticated = fmt.Errorf("endpoint requires authentication")
)

// getRoute is a utility function for making the web request to a route.
// Protocol is one of http, https, ws, or wss.  If the protocol is https or wss,
// then getRoute will make a secure transport client with InsecureSkipVerify:
// true.  If the protocol is http or ws, then getRoute does an unencrypted HTTP
// client request.  If the protocol is ws or wss, then getRoute will upgrade the
// connection to websockets and then send expectedResponse *to* the route, with
// the expectation that the route will echo back what it receives.  Note that
// getRoute returns only the first len(expectedResponse) bytes of the actual
// response.
func getRoute(routerUrl string, hostName string, protocol string, headers map[string]string, expectedResponse string) (response string, err error) {
	url := protocol + "://" + routerUrl
	var tlsConfig *tls.Config

	if protocol == "https" || protocol == "wss" {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         hostName,
		}
	}

	switch protocol {
	case "http", "https":
		httpClient := &http.Client{Transport: knet.SetTransportDefaults(&http.Transport{
			TLSClientConfig: tlsConfig,
		}),
		}
		req, err := http.NewRequest("GET", url, nil)

		if err != nil {
			return "", err
		}

		for name, value := range headers {
			req.Header.Set(name, value)
		}

		req.Host = hostName
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == 503:
			return "", ErrUnavailable
		case resp.StatusCode == 401:
			return "", ErrUnauthenticated
		case resp.StatusCode >= 400:
			return "", fmt.Errorf("GET of %s returned: %d", url, resp.StatusCode)
		}
		respBody, err := ioutil.ReadAll(resp.Body)
		cookies := resp.Cookies()
		for _, cookie := range cookies {
			if len(cookie.Name) != 32 || len(cookie.Value) != 32 {
				return "", fmt.Errorf("GET of %s returned bad cookie %s=%s", url, cookie.Name, cookie.Value)
			}
		}
		return string(respBody), err

	case "ws", "wss":
		origin := fmt.Sprintf("http://%s/", tr.GetDefaultLocalAddress())
		wsConfig, err := websocket.NewConfig(url, origin)
		if err != nil {
			return "", err
		}

		port := 80
		if protocol == "wss" {
			port = 443
		}
		wsConfig.Location.Host = fmt.Sprintf("%s:%d", hostName, port)
		wsConfig.TlsConfig = tlsConfig

		ws, err := websocket.DialConfig(wsConfig)
		if err != nil {
			if derr, ok := err.(*websocket.DialError); ok {
				if derr.Err == websocket.ErrBadStatus {
					// a better websocket library would know the difference here
					return "", ErrUnavailable
				}
			}
			return "", err
		}

		_, err = ws.Write([]byte(expectedResponse))
		if err != nil {
			return "", err
		}

		var msg = make([]byte, len(expectedResponse))
		_, err = ws.Read(msg)
		if err != nil {
			return "", err
		}

		return string(msg), nil
	}

	return "", errors.New("Unrecognized protocol in getRoute")
}

// waitForRoute loops until the client returns the expected response or an error was encountered.
func waitForRoute(routerUrl string, hostName string, protocol string, headers map[string]string, expectedResponse string) error {
	var lastErr error
	err := wait.Poll(time.Millisecond*100, 30*time.Second, func() (bool, error) {
		lastErr = nil
		resp, err := getRoute(routerUrl, hostName, protocol, headers, expectedResponse)
		if err == nil {
			if len(expectedResponse) > 0 && resp != expectedResponse {
				lastErr = fmt.Errorf("expected %q but got %q from %s://%s", expectedResponse, resp, protocol, hostName)
				return false, nil
			}
			return true, nil
		}
		if err == ErrUnavailable || strings.Contains(err.Error(), "connection refused") ||
			strings.Contains(err.Error(), "use of closed network connection") {
			return false, nil
		}
		return false, err
	})
	if err == wait.ErrWaitTimeout && lastErr != nil {
		err = lastErr
	}
	return err
}

// eventString marshals the event into a string
func eventString(e *watch.Event) string {
	obj, _ := watchjson.Object(kapi.Codecs.LegacyCodec(v1beta3.SchemeGroupVersion), e)
	s, _ := json.Marshal(obj)
	return string(s)
}

// createAndStartRouterContainer is responsible for deploying the router image in docker.  It assumes that all router images
// will use a command line flag that can take --master which points to the master url
func createAndStartRouterContainer(dockerCli *dockerClient.Client, masterIp string, routerStatsPort int, reloadInterval int) (containerId string, err error) {
	ports := []string{"80", "443"}
	if routerStatsPort > 0 {
		ports = append(ports, fmt.Sprintf("%d", routerStatsPort))
	}

	portBindings := make(map[dockerClient.Port][]dockerClient.PortBinding)
	exposedPorts := map[dockerClient.Port]struct{}{}

	for _, p := range ports {
		dockerPort := dockerClient.Port(p + "/tcp")

		portBindings[dockerPort] = []dockerClient.PortBinding{
			{
				HostPort: p,
			},
		}

		exposedPorts[dockerPort] = struct{}{}
	}

	copyEnv := []string{
		"ROUTER_EXTERNAL_HOST_HOSTNAME",
		"ROUTER_EXTERNAL_HOST_USERNAME",
		"ROUTER_EXTERNAL_HOST_PASSWORD",
		"ROUTER_EXTERNAL_HOST_HTTP_VSERVER",
		"ROUTER_EXTERNAL_HOST_HTTPS_VSERVER",
		"ROUTER_EXTERNAL_HOST_INSECURE",
		"ROUTER_EXTERNAL_HOST_PRIVKEY",
	}

	env := []string{
		fmt.Sprintf("STATS_PORT=%d", routerStatsPort),
		fmt.Sprintf("STATS_USERNAME=%s", statsUser),
		fmt.Sprintf("STATS_PASSWORD=%s", statsPassword),
	}

	reloadIntVar := fmt.Sprintf("RELOAD_INTERVAL=%ds", reloadInterval)
	env = append(env, reloadIntVar)

	for _, name := range copyEnv {
		val := os.Getenv(name)
		if len(val) > 0 {
			env = append(env, name+"="+val)
		}
	}

	vols := ""
	hostVols := []string{}

	privkeyFilename := os.Getenv("ROUTER_EXTERNAL_HOST_PRIVKEY")
	if len(privkeyFilename) != 0 {
		vols = privkeyFilename
		privkeyBindmount := fmt.Sprintf("%[1]s:%[1]s", privkeyFilename)
		hostVols = append(hostVols, privkeyBindmount)
	}

	binary := os.Getenv("ROUTER_OPENSHIFT_BINARY")
	if len(binary) != 0 {
		hostVols = append(hostVols, fmt.Sprintf("%[1]s:/usr/bin/openshift", binary))
	}

	containerOpts := dockerClient.CreateContainerOptions{
		Config: &dockerClient.Config{
			Image:        getRouterImage(),
			Cmd:          []string{"--master=" + masterIp, "--loglevel=4"},
			Env:          env,
			ExposedPorts: exposedPorts,
			VolumesFrom:  vols,
		},
		HostConfig: &dockerClient.HostConfig{
			Binds: hostVols,
		},
	}

	container, err := dockerCli.CreateContainer(containerOpts)

	if err != nil {
		return "", err
	}

	dockerHostCfg := &dockerClient.HostConfig{NetworkMode: "host", PortBindings: portBindings}
	err = dockerCli.StartContainer(container.ID, dockerHostCfg)

	if err != nil {
		return "", err
	}

	//wait for it to start
	if err := wait.Poll(time.Millisecond*100, time.Second*30, func() (bool, error) {
		c, err := dockerCli.InspectContainer(container.ID)
		if err != nil {
			return false, err
		}
		return c.State.Running, nil
	}); err != nil {
		return "", err
	}
	return container.ID, nil
}

// validateServer performs a basic run through by validating each of the configured urls for the simulator to
// ensure they are responding
func validateServer(server *tr.TestHttpService, t *testing.T) {
	_, err := http.Get("http://" + server.MasterHttpAddr)

	if err != nil {
		t.Errorf("Error validating master addr %s : %v", server.MasterHttpAddr, err)
	}

	_, err = http.Get("http://" + server.PodHttpAddr)

	if err != nil {
		t.Errorf("Error validating master addr %s : %v", server.MasterHttpAddr, err)
	}

	secureTransport := knet.SetTransportDefaults(&http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}})
	secureClient := &http.Client{Transport: secureTransport}
	_, err = secureClient.Get("https://" + server.PodHttpsAddr)

	if err != nil {
		t.Errorf("Error validating master addr %s : %v", server.MasterHttpAddr, err)
	}
}

// cleanUp stops and removes the deployed router
func cleanUp(t *testing.T, dockerCli *dockerClient.Client, routerId string) {
	dockerCli.StopContainer(routerId, 5)
	if t.Failed() {
		dockerCli.Logs(dockerClient.LogsOptions{
			Container:    routerId,
			OutputStream: os.Stdout,
			ErrorStream:  os.Stderr,
			Stdout:       true,
			Stderr:       true,
		})
	}

	dockerCli.RemoveContainer(dockerClient.RemoveContainerOptions{
		ID:    routerId,
		Force: true,
	})
}

// getRouterImage is a utility that provides the router image to use by checking to see if OPENSHIFT_ROUTER_IMAGE is set
// or by using the default image
func getRouterImage() string {
	i := os.Getenv("OPENSHIFT_ROUTER_IMAGE")

	if len(i) == 0 {
		i = defaultRouterImage
	}

	return i
}

// getRouteAddress checks for the OPENSHIFT_ROUTE_ADDRESS environment
// variable and returns it if it set and non-empty; otherwise it returns
// "0.0.0.0".
func getRouteAddress() string {
	addr := os.Getenv("OPENSHIFT_ROUTE_ADDRESS")

	if len(addr) == 0 {
		addr = "0.0.0.0"
	}

	return addr
}

// generateTestEvents generates endpoint and route added test events.
func generateTestEvents(fakeMasterAndPod *tr.TestHttpService, flag bool, serviceName, routeName, routeAlias string, endpoints []kapi.EndpointSubset) {
	endpointEvent := &watch.Event{
		Type: watch.Added,

		Object: &kapi.Endpoints{
			ObjectMeta: kapi.ObjectMeta{
				Name:      serviceName,
				Namespace: "event-brite",
			},
			Subsets: endpoints,
		},
	}

	routeEvent := &watch.Event{
		Type: watch.Added,
		Object: &routeapi.Route{
			ObjectMeta: kapi.ObjectMeta{
				Name:      routeName,
				Namespace: "event-brite",
			},
			Spec: routeapi.RouteSpec{
				Host: routeAlias,
				Path: "",
				To: kapi.ObjectReference{
					Name: serviceName,
				},
				TLS: nil,
			},
		},
	}

	if flag {
		//clean up
		routeEvent.Type = watch.Deleted
		endpointEvent.Type = watch.Modified
		endpoints := endpointEvent.Object.(*kapi.Endpoints)
		endpoints.Subsets = []kapi.EndpointSubset{}
	}

	fakeMasterAndPod.EndpointChannel <- eventString(endpointEvent)
	fakeMasterAndPod.RouteChannel <- eventString(routeEvent)
}

// TestRouterReloadCoalesce tests that router reloads are coalesced.
func TestRouterReloadCoalesce(t *testing.T) {
	//create a server which will act as a user deployed application that
	//serves http and https as well as act as a master to simulate watches
	fakeMasterAndPod := tr.NewTestHttpService()
	defer fakeMasterAndPod.Stop()

	err := fakeMasterAndPod.Start()
	validateServer(fakeMasterAndPod, t)

	if err != nil {
		t.Fatalf("Unable to start http server: %v", err)
	}

	//deploy router docker container
	dockerCli, err := testutil.NewDockerClient()

	if err != nil {
		t.Fatalf("Unable to get docker client: %v", err)
	}

	reloadInterval := 7

	routerId, err := createAndStartRouterContainer(dockerCli, fakeMasterAndPod.MasterHttpAddr, statsPort, reloadInterval)

	if err != nil {
		t.Fatalf("Error starting container %s : %v", getRouterImage(), err)
	}

	defer cleanUp(t, dockerCli, routerId)

	httpEndpoint, err := getEndpoint(fakeMasterAndPod.PodHttpAddr)
	if err != nil {
		t.Fatalf("Couldn't get http endpoint: %v", err)
	}
	_, err = getEndpoint(fakeMasterAndPod.PodHttpsAddr)
	if err != nil {
		t.Fatalf("Couldn't get https endpoint: %v", err)
	}
	_, err = getEndpoint(fakeMasterAndPod.AlternatePodHttpAddr)
	if err != nil {
		t.Fatalf("Couldn't get http endpoint: %v", err)
	}

	routeAddress := getRouteAddress()

	routeAlias := "www.example.test"
	serviceName := "example"
	endpoints := []kapi.EndpointSubset{httpEndpoint}
	numRoutes := 10

	for i := 1; i <= numRoutes; i++ {
		routeName := fmt.Sprintf("coalesce-route-%v", i)
		routeAlias = fmt.Sprintf("www.example-coalesce-%v.test", i)

		// Send the add events.
		generateTestEvents(fakeMasterAndPod, false, serviceName, routeName, routeAlias, endpoints)
	}

	// Wait for the last routeAlias to become available.
	if err := waitForRoute(routeAddress, routeAlias, "http", nil, tr.HelloPod); err != nil {
		t.Fatal(err)
	}

	// And ensure all the coalesce route aliases are available.
	for i := 1; i <= numRoutes; i++ {
		routeAlias := fmt.Sprintf("www.example-coalesce-%v.test", i)
		if err := waitForRoute(routeAddress, routeAlias, "http", nil, tr.HelloPod); err != nil {
			t.Fatalf("Unable to verify response for %q: %v", routeAlias, err)
		}
	}

	for i := 1; i <= numRoutes; i++ {
		routeName := fmt.Sprintf("coalesce-route-%v", i)
		routeAlias = fmt.Sprintf("www.example-coalesce-%v.test", i)

		// Send the cleanup events.
		generateTestEvents(fakeMasterAndPod, true, serviceName, routeName, routeAlias, endpoints)
	}

	// Wait for the first routeAlias to become unavailable.
	routeAlias = "www.example-coalesce-1.test"
	if err := wait.Poll(time.Millisecond*100, time.Duration(reloadInterval)*2*time.Second, func() (bool, error) {
		if _, err := getRoute(routeAddress, routeAlias, "http", nil, tr.HelloPod); err != nil {
			return true, nil
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Route did not become unavailable: %v", err)
	}

	// And ensure all the route aliases are gone.
	for i := 1; i <= numRoutes; i++ {
		routeAlias := fmt.Sprintf("www.example-coalesce-%v.test", i)
		if _, err := getRoute(routeAddress, routeAlias, "http", nil, tr.HelloPod); err != ErrUnavailable {
			t.Errorf("Unable to verify route deletion for %q: %+v", routeAlias, err)
		}
	}
}

// waitForRouterToBecomeAvailable checks for the router start up and waits
// till it becomes available.
func waitForRouterToBecomeAvailable(host string, port int) {
	hostAndPort := fmt.Sprintf("%s:%d", host, port)
	uri := fmt.Sprintf("%s/healthz", hostAndPort)
	waitForRoute(uri, hostAndPort, "http", nil, "")
}
