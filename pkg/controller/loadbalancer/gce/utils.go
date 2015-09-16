/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package lb

import (
	"k8s.io/kubernetes/pkg/client/cache"
)

// poolStore is used as a cache for cluster resource pools.
type poolStore struct {
	cache.ThreadSafeStore
}

// Returns a read only copy of the k:v pairs in the store.
// Caller beware: Violates traditional snapshot guarantees.
func (p *poolStore) snapshot() map[string]interface{} {
	snap := map[string]interface{}{}
	for _, key := range p.ListKeys() {
		if item, ok := p.Get(key); ok {
			snap[key] = item
		}
	}
	return snap
}

func newPoolStore() *poolStore {
	return &poolStore{
		cache.NewThreadSafeStore(cache.Indexers{}, cache.Indices{})}
}

// compareLinks returns true if the 2 self links are equal.
func compareLinks(l1, l2 string) bool {
	// TODO: These can be partial links
	return l1 == l2 && l1 != ""
}
