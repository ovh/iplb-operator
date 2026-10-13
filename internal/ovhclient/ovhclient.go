/*
Copyright 2026.

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

// Package ovhclient builds and caches OVHcloud API clients from OAuth2
// credentials stored in Kubernetes Secrets.
package ovhclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/ovh/go-ovh/ovh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
)

// Secret keys holding the OAuth2 credentials.
const (
	KeyClientID     = "clientID"
	KeyClientSecret = "clientSecret"
)

// Client is the minimal surface of the OVHcloud API we depend on. It is
// satisfied by *ovh.Client and faked in tests.
type Client interface {
	Get(url string, resType interface{}) error
	Post(url string, reqBody, resType interface{}) error
	Put(url string, reqBody, resType interface{}) error
	Delete(url string, resType interface{}) error
}

// Factory resolves a ServiceRef to an authenticated OVHcloud API Client.
type Factory interface {
	ClientFor(ctx context.Context, ref iplbv1alpha1.ServiceRef, defaultNamespace string) (Client, error)
}

// builderFunc lets tests substitute the underlying go-ovh constructor.
type builderFunc func(endpoint, clientID, clientSecret string) (Client, error)

func defaultBuilder(endpoint, clientID, clientSecret string) (Client, error) {
	return ovh.NewOAuth2Client(endpoint, clientID, clientSecret)
}

// secretFactory reads OAuth2 credentials from a Secret and builds (and caches)
// an OVHcloud client per credential set.
type secretFactory struct {
	reader  client.Reader
	build   builderFunc
	mu      sync.Mutex
	clients map[string]Client // keyed by endpoint+credential hash
}

// NewFactory returns a Factory that reads credentials with the given reader
// (typically the manager's API client) and builds real go-ovh OAuth2 clients.
func NewFactory(reader client.Reader) Factory {
	return &secretFactory{reader: reader, build: defaultBuilder, clients: map[string]Client{}}
}

// NewFactoryWithBuilder is used by tests to inject a fake client builder.
func NewFactoryWithBuilder(reader client.Reader, build builderFunc) Factory {
	return &secretFactory{reader: reader, build: build, clients: map[string]Client{}}
}

func (f *secretFactory) ClientFor(ctx context.Context, ref iplbv1alpha1.ServiceRef, defaultNamespace string) (Client, error) {
	endpoint := ref.Endpoint
	if endpoint == "" {
		endpoint = "ovh-eu"
	}

	ns := ref.CredentialsSecretRef.Namespace
	if ns == "" {
		ns = defaultNamespace
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: ns, Name: ref.CredentialsSecretRef.Name}
	if err := f.reader.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("reading credentials secret %s: %w", key, err)
	}

	clientID := string(secret.Data[KeyClientID])
	clientSecret := string(secret.Data[KeyClientSecret])
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("secret %s is missing required keys %q and/or %q", key, KeyClientID, KeyClientSecret)
	}

	// Cache key includes the credential material so rotation transparently
	// produces a fresh client.
	sum := sha256.Sum256([]byte(endpoint + "\x00" + clientID + "\x00" + clientSecret))
	cacheKey := hex.EncodeToString(sum[:])

	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[cacheKey]; ok {
		return c, nil
	}

	c, err := f.build(endpoint, clientID, clientSecret)
	if err != nil {
		return nil, fmt.Errorf("building OVHcloud client for endpoint %q: %w", endpoint, err)
	}
	f.clients[cacheKey] = c
	return c, nil
}
