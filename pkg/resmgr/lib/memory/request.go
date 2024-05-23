// Copyright The NRI Plugins Authors. All Rights Reserved.
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

package libmem

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
)

// Request represents a memory allocation request.
type Request struct {
	id       string   // unique ID for the request, typically a container ID
	name     string   // an optional, user provided name for the request
	limit    int64    // the amount of memory to allocate
	affinity NodeMask // nodes to start allocating memory from
	types    TypeMask // types of nodes to use for fulfilling the request
	strict   bool     // strict preference for types
	inertia  Inertia  // larger inertia results in more reluctance to move a request
	zone     NodeMask // the nodes allocated for the request, ideally == affinity
}

// Inertia describes reluctancy against moving an allocation.
type Inertia int

const (
	NoInertia   Inertia = iota // can be moved around freely
	BestEffort  Inertia = iota // go ahead, do your worst
	Burstable                  // move if necessary
	Guaranteed                 // try to avoid moving this
	Preserved                  // try harder to avoid moving this
	Reservation                // immovable once allocated
)

// RequestOption is an opaque option which can be applied to a request.
type RequestOption func(*Request)

// Container is a convenience function to create a request for a container
// of a particular QoS class. The QoS class is used to set the inertia for
// the request.
func Container(id, name string, qos string, limit int64, affin NodeMask) *Request {
	opts := []RequestOption{
		WithName(name),
		WithQosClass(qos),
	}
	return NewRequest(id, limit, affin, opts...)
}

// ContainerWithTypes is a convenience function to create a request with a
// memory type preference for a container of a particular QoS class. The QoS
// class is used to set the inertia for the request.
func ContainerWithTypes(id, name, qos string, limit int64, affin NodeMask, types TypeMask) *Request {
	opts := []RequestOption{
		WithName(name),
		WithQosClass(qos),
		WithPreferredTypes(types),
	}
	return NewRequest(id, limit, affin, opts...)
}

// ContainerWithStrictTypes is a convenience function to create a request
// with strict memory type preference for a container of a particular QoS
// class. The QoS class is used to set the inertia for the request.
func ContainerWithStrictTypes(id, name, qos string, limit int64, affin NodeMask, types TypeMask) *Request {
	opts := []RequestOption{
		WithName(name),
		WithQosClass(qos),
		WithStrictTypes(types),
	}
	return NewRequest(id, limit, affin, opts...)
}

// PreserveContainer is a convenience function to create an allocation
// request for a container with 'preserved' memory. Such a request has
// higher inertia than other, ordinary requests. The allocator tries to
// avoid moving such allocations later when trying to satisfy subsequent
// requests.
func PreservedContainer(id, name string, limit int64, affin NodeMask) *Request {
	opts := []RequestOption{
		WithName(name),
		WithInertia(Preserved),
	}
	return NewRequest(id, limit, affin, opts...)
}

// ReservedMemory is a convenience function to create an allocation request
// for reserving memory from the allocator. Once memory has been succesfully
// reserved, it is never moved by the allocator. ReservedMemory can be used
// to inform the allocator about external memory allocations which are beyond
// the control of the caller.
func ReservedMemory(limit int64, nodes NodeMask, options ...RequestOption) *Request {
	var (
		id   = NewID()
		name = "memory reservation #" + id
		opts = append([]RequestOption{WithName(name)}, options...)
	)
	return NewRequest(id, limit, nodes, append(opts, WithInertia(Reservation))...)
}

// WithPreferredTypes returns an option to set preferred memory types for a request.
func WithPreferredTypes(types TypeMask) RequestOption {
	return func(r *Request) {
		r.types = types
	}
}

// WithStrictTypes returns an option to set strict memory types for a request.
func WithStrictTypes(types TypeMask) RequestOption {
	return func(r *Request) {
		r.types = types
		r.strict = true
	}
}

// WithInertia returns an option to set the inertia of a request.
func WithInertia(i Inertia) RequestOption {
	return func(r *Request) {
		r.inertia = i
	}
}

// WithQosClass returns an option to set the inertia of a request based on a container QoS class.
func WithQosClass(qosClass string) RequestOption {
	switch strings.ToLower(qosClass) {
	case "besteffort":
		return WithInertia(NoInertia)
	case "burstable":
		return WithInertia(Burstable)
	case "guaranteed":
		return WithInertia(Guaranteed)
	default:
		return WithInertia(NoInertia)
	}
}

// WithName returns an option for setting a verbose name for the request.
func WithName(name string) RequestOption {
	return func(r *Request) {
		r.name = name
	}
}

// NewRequest returns a new request with the given parameters and options.
func NewRequest(id string, limit int64, affinity NodeMask, options ...RequestOption) *Request {
	r := &Request{
		id:       id,
		limit:    limit,
		affinity: affinity,
	}

	if limit > 0 {
		r.inertia = Burstable
	}

	for _, o := range options {
		o(r)
	}

	return r
}

// ID returns the ID of this request.
func (r *Request) ID() string {
	return r.id
}

// Name returns the name of this request, using its ID if a name was not set.
func (r *Request) Name() string {
	if r.name != "" {
		return r.name
	} else {
		return "ID:#" + r.id
	}
}

// String returns a string representation of this request.
func (r *Request) String() string {
	var (
		kind string
		size = HumanReadableSize(r.Size())
		name = r.Name()
	)

	switch r.inertia {
	case NoInertia:
		kind = "besteffort workload"
	case Burstable:
		kind = "burstable workload"
	case Guaranteed:
		kind = "guaranteed workload"
	case Preserved:
		kind = "preserved workload"
	case Reservation:
		kind = "memory reservation"
	}

	if size == "0" {
		size = ""
	} else {
		size = ", size " + size
	}

	return kind + "<" + name + size + ">"
}

// Size returns the allocation size of this request.
func (r *Request) Size() int64 {
	return r.limit
}

// Affinity returns the node affinity of this request. The allocator
// will start with this set of nodes to fulfill the request.
func (r *Request) Affinity() NodeMask {
	return r.affinity
}

// Types returns the types of memory for this request.
func (r *Request) Types() TypeMask {
	return r.types
}

// IsStrict returns whether the type preference for this request is strict.
func (r *Request) IsStrict() bool {
	return r.strict
}

// Inertia returns the inertia for this request.
func (r *Request) Inertia() Inertia {
	return r.inertia
}

// Zone returns the allocated memory zone for this request. It is
// the final set of nodes the allocator used to fulfill the request.
func (r *Request) Zone() NodeMask {
	return r.zone
}

// String returns a string representation of this inertia.
func (i Inertia) String() string {
	switch i {
	case NoInertia:
		return "NoInertia"
	case Burstable:
		return "Burstable"
	case Guaranteed:
		return "Guaranteed"
	case Preserved:
		return "Preserved"
	case Reservation:
		return "Reservation"
	}
	return fmt.Sprintf("%%(libmem:BadInertia=%d)", i)
}

var (
	nextID atomic.Int64
)

// NewID returns a new internally unique ID. It is used to generate
// default IDs for memory reservations if it is not provided by the
// caller.
func NewID() string {
	return "request-id-" + strconv.FormatInt(nextID.Add(1), 16)
}

// SortRequestsByInertiaAndSize is a helper to sort requests by increasing
// inertia, size, and request ID in this order.
func SortRequestsByInertiaAndSize(a, b *Request) int {
	if a.Inertia() < b.Inertia() {
		return -1
	}
	if b.Inertia() < a.Inertia() {
		return 1
	}

	if diff := b.Size() - a.Size(); diff < 0 {
		return -1
	} else if diff > 0 {
		return 1
	}

	// TODO(klihub): instead of using ID() add an internal index/age and use
	// that as the last resort for providing a stable sorting order.

	return strings.Compare(a.ID(), b.ID())
}

// HumanReadableSize returns the given size as a human-readable string.
func HumanReadableSize(size int64) string {
	if size >= 1024 {
		units := []string{"k", "M", "G", "T"}

		for i, d := 0, int64(1024); i < len(units); i, d = i+1, d<<10 {
			if val := size / d; 1 <= val && val < 1024 {
				if fval := float64(size) / float64(d); math.Floor(fval) != fval {
					return fmt.Sprintf("%.3g%s", fval, units[i])
				} else {
					return fmt.Sprintf("%d%s", val, units[i])
				}
			}
		}
	}

	return strconv.FormatInt(size, 10)
}

func prettySize(v int64) string {
	return HumanReadableSize(v)
}
