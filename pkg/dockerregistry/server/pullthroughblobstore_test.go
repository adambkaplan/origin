package server

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/docker/distribution"
	"github.com/docker/distribution/configuration"
	"github.com/docker/distribution/context"
	"github.com/docker/distribution/digest"
	"github.com/docker/distribution/registry/handlers"
	_ "github.com/docker/distribution/registry/storage"
	_ "github.com/docker/distribution/registry/storage/driver/inmemory"

	"k8s.io/kubernetes/pkg/client/restclient"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	ktestclient "k8s.io/kubernetes/pkg/client/unversioned/testclient"

	osclient "github.com/openshift/origin/pkg/client"
	"github.com/openshift/origin/pkg/client/testclient"
	registrytest "github.com/openshift/origin/pkg/dockerregistry/testutil"
)

func TestPullthroughServeBlob(t *testing.T) {
	ctx := context.Background()

	testImage, err := registrytest.NewImageForManifest("user/app", registrytest.SampleImageManifestSchema1, false)
	if err != nil {
		t.Fatal(err)
	}
	client := &testclient.Fake{}
	client.AddReactor("get", "images", registrytest.GetFakeImageGetHandler(t, *testImage))

	// TODO: get rid of those nasty global vars
	backupRegistryClient := DefaultRegistryClient
	DefaultRegistryClient = makeFakeRegistryClient(client, ktestclient.NewSimpleFake())
	defer func() {
		// set it back once this test finishes to make other unit tests working again
		DefaultRegistryClient = backupRegistryClient
	}()

	// pullthrough middleware will attempt to pull from this registry instance
	remoteRegistryApp := handlers.NewApp(ctx, &configuration.Configuration{
		Loglevel: "debug",
		Storage: configuration.Storage{
			"inmemory": configuration.Parameters{},
			"cache": configuration.Parameters{
				"blobdescriptor": "inmemory",
			},
			"delete": configuration.Parameters{
				"enabled": true,
			},
		},
		Middleware: map[string][]configuration.Middleware{
			"repository": {{Name: "openshift"}},
		},
	})
	remoteRegistryServer := httptest.NewServer(remoteRegistryApp)
	defer remoteRegistryServer.Close()

	serverURL, err := url.Parse(remoteRegistryServer.URL)
	if err != nil {
		t.Fatalf("error parsing server url: %v", err)
	}
	os.Setenv("DOCKER_REGISTRY_URL", serverURL.Host)
	testImage.DockerImageReference = fmt.Sprintf("%s/%s@%s", serverURL.Host, "user/app", testImage.Name)

	testImageStream := registrytest.TestNewImageStreamObject("user", "app", "latest", testImage.Name, testImage.DockerImageReference)
	client.AddReactor("get", "imagestreams", registrytest.GetFakeImageStreamGetHandler(t, *testImageStream))

	blob1Desc, blob1Content, err := registrytest.UploadTestBlob(serverURL, nil, "user/app")
	if err != nil {
		t.Fatal(err)
	}
	blob2Desc, blob2Content, err := registrytest.UploadTestBlob(serverURL, nil, "user/app")
	if err != nil {
		t.Fatal(err)
	}

	blob1Storage := map[digest.Digest][]byte{blob1Desc.Digest: blob1Content}
	blob2Storage := map[digest.Digest][]byte{blob2Desc.Digest: blob2Content}

	for _, tc := range []struct {
		name                       string
		method                     string
		blobDigest                 digest.Digest
		localBlobs                 map[digest.Digest][]byte
		expectedStatError          error
		expectedContentLength      int64
		expectedBytesServed        int64
		expectedBytesServedLocally int64
		expectedLocalCalls         map[string]int
	}{
		{
			name:                  "stat local blob",
			method:                "HEAD",
			blobDigest:            blob1Desc.Digest,
			localBlobs:            blob1Storage,
			expectedContentLength: int64(len(blob1Content)),
			expectedLocalCalls: map[string]int{
				"Stat":      1,
				"ServeBlob": 1,
			},
		},

		{
			name:                       "serve local blob",
			method:                     "GET",
			blobDigest:                 blob1Desc.Digest,
			localBlobs:                 blob1Storage,
			expectedContentLength:      int64(len(blob1Content)),
			expectedBytesServed:        int64(len(blob1Content)),
			expectedBytesServedLocally: int64(len(blob1Content)),
			expectedLocalCalls: map[string]int{
				"Stat":      1,
				"ServeBlob": 1,
			},
		},

		{
			name:                  "stat remote blob",
			method:                "HEAD",
			blobDigest:            blob1Desc.Digest,
			localBlobs:            blob2Storage,
			expectedContentLength: int64(len(blob1Content)),
			expectedLocalCalls:    map[string]int{"Stat": 1},
		},

		{
			name:                  "serve remote blob",
			method:                "GET",
			blobDigest:            blob1Desc.Digest,
			expectedContentLength: int64(len(blob1Content)),
			expectedBytesServed:   int64(len(blob1Content)),
			expectedLocalCalls:    map[string]int{"Stat": 1},
		},

		{
			name:               "unknown blob digest",
			method:             "GET",
			blobDigest:         unknownBlobDigest,
			expectedStatError:  distribution.ErrBlobUnknown,
			expectedLocalCalls: map[string]int{"Stat": 1},
		},
	} {
		localBlobStore := newTestBlobStore(tc.localBlobs)

		cachedLayers, err := newDigestToRepositoryCache(10)
		if err != nil {
			t.Fatal(err)
		}
		ptbs := &pullthroughBlobStore{
			BlobStore: localBlobStore,
			repo: &repository{
				ctx:            ctx,
				namespace:      "user",
				name:           "app",
				pullthrough:    true,
				cachedLayers:   cachedLayers,
				registryClient: client,
			},
			digestToStore:              make(map[string]distribution.BlobStore),
			pullFromInsecureRegistries: true,
		}

		req, err := http.NewRequest(tc.method, fmt.Sprintf("http://example.org/v2/user/app/blobs/%s", tc.blobDigest), nil)
		if err != nil {
			t.Fatalf("[%s] failed to create http request: %v", tc.name, err)
		}
		w := httptest.NewRecorder()

		dgst := digest.Digest(tc.blobDigest)

		_, err = ptbs.Stat(ctx, dgst)
		if err != tc.expectedStatError {
			t.Errorf("[%s] Stat returned unexpected error: %#+v != %#+v", tc.name, err, tc.expectedStatError)
		}
		if err != nil || tc.expectedStatError != nil {
			continue
		}
		err = ptbs.ServeBlob(ctx, w, req, dgst)
		if err != nil {
			t.Errorf("[%s] unexpected ServeBlob error: %v", tc.name, err)
			continue
		}

		clstr := w.Header().Get("Content-Length")
		if cl, err := strconv.ParseInt(clstr, 10, 64); err != nil {
			t.Errorf(`[%s] unexpected Content-Length: %q != "%d"`, tc.name, clstr, tc.expectedContentLength)
		} else {
			if cl != tc.expectedContentLength {
				t.Errorf("[%s] Content-Length does not match expected size: %d != %d", tc.name, cl, tc.expectedContentLength)
			}
		}
		if w.Header().Get("Content-Type") != "application/octet-stream" {
			t.Errorf("[%s] Content-Type does not match expected: %q != %q", tc.name, w.Header().Get("Content-Type"), "application/octet-stream")
		}

		body := w.Body.Bytes()
		if int64(len(body)) != tc.expectedBytesServed {
			t.Errorf("[%s] unexpected size of body: %d != %d", tc.name, len(body), tc.expectedBytesServed)
		}

		for name, expCount := range tc.expectedLocalCalls {
			count := localBlobStore.calls[name]
			if count != expCount {
				t.Errorf("[%s] expected %d calls to method %s of local blob store, not %d", tc.name, expCount, name, count)
			}
		}
		for name, count := range localBlobStore.calls {
			if _, exists := tc.expectedLocalCalls[name]; !exists {
				t.Errorf("[%s] expected no calls to method %s of local blob store, got %d", tc.name, name, count)
			}
		}

		if localBlobStore.bytesServed != tc.expectedBytesServedLocally {
			t.Errorf("[%s] unexpected number of bytes served locally: %d != %d", tc.name, localBlobStore.bytesServed, tc.expectedBytesServed)
		}
	}
}

const (
	unknownBlobDigest = "sha256:bef57ec7f53a6d40beb640a780a639c83bc29ac8a9816f1fc6c5c6dcd93c4721"
)

func makeDigestFromBytes(data []byte) digest.Digest {
	return digest.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(data)))
}

type testBlobStore struct {
	// blob digest mapped to content
	blobs map[digest.Digest][]byte
	// method name mapped to number of invocations
	calls       map[string]int
	bytesServed int64
}

var _ distribution.BlobStore = &testBlobStore{}

func newTestBlobStore(blobs map[digest.Digest][]byte) *testBlobStore {
	b := make(map[digest.Digest][]byte)
	for d, content := range blobs {
		b[d] = content
	}
	return &testBlobStore{
		blobs: b,
		calls: make(map[string]int),
	}
}

func (t *testBlobStore) Stat(ctx context.Context, dgst digest.Digest) (distribution.Descriptor, error) {
	t.calls["Stat"]++
	content, exists := t.blobs[dgst]
	if !exists {
		return distribution.Descriptor{}, distribution.ErrBlobUnknown
	}
	return distribution.Descriptor{
		Size:   int64(len(content)),
		Digest: makeDigestFromBytes(content),
	}, nil
}

func (t *testBlobStore) Get(ctx context.Context, dgst digest.Digest) ([]byte, error) {
	t.calls["Get"]++
	content, exists := t.blobs[dgst]
	if !exists {
		return nil, distribution.ErrBlobUnknown
	}
	return content, nil
}

func (t *testBlobStore) Enumerate(ctx context.Context, ingester func(digest.Digest) error) error {
	return fmt.Errorf("method not implemented")
}

func (t *testBlobStore) Open(ctx context.Context, dgst digest.Digest) (distribution.ReadSeekCloser, error) {
	t.calls["Open"]++
	content, exists := t.blobs[dgst]
	if !exists {
		return nil, distribution.ErrBlobUnknown
	}
	return &testBlobFileReader{
		bs:      t,
		content: content,
	}, nil
}

func (t *testBlobStore) Put(ctx context.Context, mediaType string, p []byte) (distribution.Descriptor, error) {
	t.calls["Put"]++
	return distribution.Descriptor{}, fmt.Errorf("method not implemented")
}

func (t *testBlobStore) Create(ctx context.Context) (distribution.BlobWriter, error) {
	t.calls["Create"]++
	return nil, fmt.Errorf("method not implemented")
}

func (t *testBlobStore) Resume(ctx context.Context, id string) (distribution.BlobWriter, error) {
	t.calls["Resume"]++
	return nil, fmt.Errorf("method not implemented")
}

func (t *testBlobStore) ServeBlob(ctx context.Context, w http.ResponseWriter, req *http.Request, dgst digest.Digest) error {
	t.calls["ServeBlob"]++
	content, exists := t.blobs[dgst]
	if !exists {
		return distribution.ErrBlobUnknown
	}
	reader := bytes.NewReader(content)
	setResponseHeaders(w, int64(len(content)), "application/octet-stream", dgst)
	http.ServeContent(w, req, dgst.String(), time.Time{}, reader)
	n, err := reader.Seek(0, 1)
	if err != nil {
		return err
	}
	t.bytesServed = n
	return nil
}

func (t *testBlobStore) Delete(ctx context.Context, dgst digest.Digest) error {
	t.calls["Delete"]++
	return fmt.Errorf("method not implemented")
}

type testBlobFileReader struct {
	bs      *testBlobStore
	content []byte
	offset  int64
}

var _ distribution.ReadSeekCloser = &testBlobFileReader{}

func (fr *testBlobFileReader) Read(p []byte) (n int, err error) {
	fr.bs.calls["ReadSeakCloser.Read"]++
	n = copy(p, fr.content[fr.offset:])
	fr.offset += int64(n)
	fr.bs.bytesServed += int64(n)
	return n, nil
}

func (fr *testBlobFileReader) Seek(offset int64, whence int) (int64, error) {
	fr.bs.calls["ReadSeakCloser.Seek"]++

	newOffset := fr.offset

	switch whence {
	case os.SEEK_CUR:
		newOffset += int64(offset)
	case os.SEEK_END:
		newOffset = int64(len(fr.content)) + offset
	case os.SEEK_SET:
		newOffset = int64(offset)
	}

	var err error
	if newOffset < 0 {
		err = fmt.Errorf("cannot seek to negative position")
	} else {
		// No problems, set the offset.
		fr.offset = newOffset
	}

	return fr.offset, err
}

func (fr *testBlobFileReader) Close() error {
	fr.bs.calls["ReadSeakCloser.Close"]++
	return nil
}

type testBlobDescriptorManager struct {
	mu              sync.Mutex
	cond            *sync.Cond
	stats           map[string]int
	unsetRepository bool
}

// NewTestBlobDescriptorManager allows to control blobDescriptorService and collects statistics of called
// methods.
func NewTestBlobDescriptorManager() *testBlobDescriptorManager {
	m := &testBlobDescriptorManager{
		stats: make(map[string]int),
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *testBlobDescriptorManager) clearStats() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for k := range m.stats {
		delete(m.stats, k)
	}
}

func (m *testBlobDescriptorManager) methodInvoked(methodName string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	newCount := m.stats[methodName] + 1
	m.stats[methodName] = newCount
	m.cond.Signal()

	return newCount
}

// unsetRepository returns true if the testBlobDescriptorService should unset repository from context before
// passing down the call
func (m *testBlobDescriptorManager) getUnsetRepository() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.unsetRepository
}

// changeUnsetRepository allows to configure whether the testBlobDescriptorService should unset repository
// from context before passing down the call
func (m *testBlobDescriptorManager) changeUnsetRepository(unset bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.unsetRepository = unset
}

// getStats waits until blob descriptor service's methods are called specified number of times and returns
// collected numbers of invocations per each method watched. An error will be returned if a given timeout is
// reached without satisfying minimum limit.s
func (m *testBlobDescriptorManager) getStats(minimumLimits map[string]int, timeout time.Duration) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var err error
	end := time.Now().Add(timeout)

	if len(minimumLimits) > 0 {
	Loop:
		for !statsGreaterThanOrEqual(m.stats, minimumLimits) {
			c := make(chan struct{})
			go func() { m.cond.Wait(); c <- struct{}{} }()

			now := time.Now()
			select {
			case <-time.After(end.Sub(now)):
				err = fmt.Errorf("timeout while waiting on expected stats")
				break Loop
			case <-c:
				continue Loop
			}
		}
	}

	stats := make(map[string]int)
	for k, v := range m.stats {
		stats[k] = v
	}

	return stats, err
}

func statsGreaterThanOrEqual(stats, minimumLimits map[string]int) bool {
	for key, val := range minimumLimits {
		if val > stats[key] {
			return false
		}
	}
	return true
}

func makeFakeRegistryClient(client osclient.Interface, kClient kclient.Interface) RegistryClient {
	return &fakeRegistryClient{
		client:  client,
		kClient: kClient,
	}
}

type fakeRegistryClient struct {
	client  osclient.Interface
	kClient kclient.Interface
}

func (f *fakeRegistryClient) Clients() (osclient.Interface, kclient.Interface, error) {
	return f.client, f.kClient, nil
}
func (f *fakeRegistryClient) SafeClientConfig() restclient.Config {
	return (&registryClient{}).SafeClientConfig()
}
