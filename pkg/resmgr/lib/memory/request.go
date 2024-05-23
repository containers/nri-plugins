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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Request represents a memory allocation request.
type Request struct {
	id       string   // unique ID for the request, typically a container ID
	name     string   // an optional, user provided name for the request
	limit    int64    // the amount of memory to allocate
	affinity NodeMask // nodes to start allocating memory from
	types    TypeMask // types of nodes to use for fulfilling the request
	strict   bool     // strict preference for types
	priority Priority // larger priority means more reluctance to move a request
	zone     NodeMask // the nodes allocated for the request, ideally == affinity
	created  int64    // timestamp of creation for this request
}

// Priority describes the priority of a request. Its is used to choose which
// requests should be reassigned to another memory zone when a zone runs out
// of memory (is overcommitted) by affinity based assignments alone. Priority
// is always relative to other requests. Default priorities are provided for
// the well-known QoS classes (BestEffort, Burstable, Guaranteed).
type Priority int16

const (
	NoPriority  Priority = 0             // moved around freely
	BestEffort  Priority = 0             // ditto, do your worst
	Burstable   Priority = (1 << 10)     // move if necessary
	Guaranteed  Priority = (1 << 14)     // try avoid moving this
	Preserved   Priority = (1 << 15) - 2 // try avoid moving this... harder
	Reservation Priority = (1 << 15) - 1 // immovable
)

// RequestOption is an opaque option which can be applied to a request.
type RequestOption func(*Request)

// Container is a convenience function to create a request for a container
// of a particular QoS class. The QoS class is used to set the priority for
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
// class is used to set the priority for the request.
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
// class. The QoS class is used to set the priority for the request.
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
// higher priority than other, ordinary requests. The allocator tries to
// avoid moving such allocations later when trying to satisfy subsequent
// requests.
func PreservedContainer(id, name string, limit int64, affin NodeMask) *Request {
	opts := []RequestOption{
		WithName(name),
		WithPriority(Preserved),
	}
	return NewRequest(id, limit, affin, opts...)
}

// ReservedMemory is a convenience function to create an allocation request
// for reserving memory from the allocator. Once memory has been successfully
// reserved, it is never moved by the allocator. ReservedMemory can be used
// to inform the allocator about external memory allocations which are beyond
// the control of the caller.
func ReservedMemory(limit int64, nodes NodeMask, options ...RequestOption) *Request {
	var (
		id   = NewID()
		name = "memory reservation #" + id
		opts = append([]RequestOption{WithName(name)}, options...)
	)
	return NewRequest(id, limit, nodes, append(opts, WithPriority(Reservation))...)
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

// WithPriority returns an option to set the priority of a request.
func WithPriority(p Priority) RequestOption {
	return func(r *Request) {
		r.priority = p
	}
}

// WithQosClass returns an option to set the priority of a request based on a QoS class.
func WithQosClass(qosClass string) RequestOption {
	switch strings.ToLower(qosClass) {
	case "besteffort":
		return WithPriority(BestEffort)
	case "burstable":
		return WithPriority(Burstable)
	case "guaranteed":
		return WithPriority(Guaranteed)
	default:
		log.Error("%v: %q", ErrInvalidQosClass, qosClass)
		return WithPriority(NoPriority)
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
		created:  time.Now().UnixNano(),
	}

	if limit > 0 {
		r.priority = Burstable
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

	switch r.priority {
	case BestEffort, Burstable, Guaranteed, Preserved:
		kind = r.priority.String() + " workload"
	case Reservation:
		kind = "memory reservation"
	default:
		kind = r.priority.String() + " workload"
	}

	if size == "0" {
		size = ""
	} else {
		size = ", size " + size
	}

	if a := r.Affinity(); a != 0 {
		aff := " affine to " + a.String()
		if size == "" {
			size = aff
		} else {
			size += "," + aff
		}
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

// Priority returns the priority for this request.
func (r *Request) Priority() Priority {
	return r.priority
}

// Zone returns the allocated memory zone for this request. It is
// the final set of nodes the allocator used to fulfill the request.
func (r *Request) Zone() NodeMask {
	return r.zone
}

// Created returns the timestamp of creation for this request.
func (r *Request) Created() int64 {
	return r.created
}

// String returns a string representation of this priority.
func (p Priority) String() string {
	switch p {
	case BestEffort:
		return "besteffort"
	case Burstable:
		return "burstable"
	case Guaranteed:
		return "guaranteed"
	case Preserved:
		return "preserved"
	case Reservation:
		return "memory reservation"
	}
	return fmt.Sprintf("priority %d", p)
}

var (
	nextID atomic.Int64
)

// NewID returns a new internally generated ID. It is used to give
// default IDs for memory reservations in case one is not provided
// by the caller.
func NewID() string {
	return "request-id-" + strconv.FormatInt(nextID.Add(1), 16)
}

// SortRequests filters the requests by a filter function into a slice,
// then sorts the slice by chaining the given sorting functions. A nil
// filter function picks all requests.
func SortRequests(requests map[string]*Request, f RequestFilter, s ...RequestSorter) []*Request {
	slice := make([]*Request, 0, len(requests))
	for _, req := range requests {
		if f == nil || f(req) {
			slice = append(slice, req)
		}
	}
	if len(s) > 0 {
		slices.SortFunc(slice, func(r1, r2 *Request) int {
			for _, fn := range s {
				if diff := fn(r1, r2); diff != 0 {
					return diff
				}
			}
			return 0
		})
	}
	return slice
}

// RequestFilter is a function to filter requests.
type RequestFilter func(*Request) bool

// RequestSorter is a function to compare requests for sorting.
type RequestSorter func(r1, r2 *Request) int

// RequestsWithMaxPriority filters requests by maximum allowed priority.
func RequestsWithMaxPriority(limit Priority) RequestFilter {
	return func(r *Request) bool {
		return r.Priority() <= limit
	}
}

// RequestsByPriority compares requests by increasing priority.
func RequestsByPriority(r1, r2 *Request) int {
	return int(r1.Priority() - r2.Priority())
}

// RequestsBySize compares requests by increasing size.
func RequestsBySize(r1, r2 *Request) int {
	return int(r1.Size() - r2.Size())
}

// RequestsByAge compares requests by increasing age.
func RequestsByAge(r1, r2 *Request) int {
	return int(r2.Created() - r1.Created())
}

// HumanReadableSize returns the given size as a human-readable string.
func HumanReadableSize(size int64) string {
	if size >= 1024 {
		units := []string{"k", "M", "G", "T"}

		for i, d := 0, int64(1024); i < len(units); i, d = i+1, d<<10 {
			if val := size / d; 1 <= val && val < 1024 {
				if fval := float64(size) / float64(d); math.Floor(fval) != fval {
					return strings.TrimRight(fmt.Sprintf("%.3f", fval), "0") + units[i]
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
