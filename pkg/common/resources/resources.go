/*
 Licensed to the Apache Software Foundation (ASF) under one
 or more contributor license agreements.  See the NOTICE file
 distributed with this work for additional information
 regarding copyright ownership.  The ASF licenses this file
 to you under the Apache License, Version 2.0 (the
 "License"); you may not use this file except in compliance
 with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package resources

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/apache/yunikorn-core/pkg/log"
	"github.com/apache/yunikorn-scheduler-interface/lib/go/common"
	"github.com/apache/yunikorn-scheduler-interface/lib/go/si"
)

type Resource struct {
	Resources map[string]Quantity
}

// No unit defined here for better performance
type Quantity int64

func (q Quantity) string() string {
	return strconv.FormatInt(int64(q), 10)
}

// Never update value of Zero
var Zero = NewResource()

func NewResource() *Resource {
	return &Resource{Resources: make(map[string]Quantity)}
}

func NewResourceFromProto(proto *si.Resource) *Resource {
	out := NewResource()
	if proto == nil {
		return out
	}
	for k, v := range proto.Resources {
		out.Resources[k] = Quantity(v.Value)
	}
	return out
}

func NewResourceFromMap(m map[string]Quantity) *Resource {
	if m == nil {
		return NewResource()
	}
	return &Resource{Resources: m}
}

// Create a new resource from a string.
// The string must be a json marshalled si.Resource.
func NewResourceFromString(str string) (*Resource, error) {
	var siRes *si.Resource
	if err := json.Unmarshal([]byte(str), &siRes); err != nil {
		return nil, err
	}
	return NewResourceFromProto(siRes), nil
}

// Create a new resource from the config map.
// The config map must have been checked before being applied. The check here is just for safety so we do not crash.
func NewResourceFromConf(configMap map[string]string) (*Resource, error) {
	res := NewResource()
	for key, strVal := range configMap {
		var intValue Quantity
		var err error
		switch key {
		case common.CPU:
			intValue, err = ParseVCore(strVal)
		default:
			intValue, err = ParseQuantity(strVal)
		}
		if err != nil {
			return nil, err
		}
		res.Resources[key] = intValue
	}
	return res, nil
}

func (r *Resource) String() string {
	if r == nil {
		return "nil resource"
	}
	return fmt.Sprintf("%v", r.Resources)
}

func (r *Resource) DAOMap() map[string]int64 {
	res := make(map[string]int64)
	if r != nil {
		for k, v := range r.Resources {
			res[k] = int64(v)
		}
	}
	return res
}

// Convert to a protobuf implementation
// a nil resource passes back an empty proto object
func (r *Resource) ToProto() *si.Resource {
	proto := &si.Resource{}
	proto.Resources = make(map[string]*si.Quantity)
	if r != nil {
		for k, v := range r.Resources {
			proto.Resources[k] = &si.Quantity{Value: int64(v)}
		}
	}
	return proto
}

// Clone returns a clone (copy) of the resource it is called on.
// This provides a deep copy of the object with the exact same member set.
// NOTE: this is a clone not a sparse copy of the original.
func (r *Resource) Clone() *Resource {
	if r == nil {
		return nil
	}
	ret := NewResource()
	for k, v := range r.Resources {
		ret.Resources[k] = v
	}
	return ret
}

// Prune removes any resource type that has a zero value set.
// Note that a zero value set and a type not being set are interpreted differently for quotas.
func (r *Resource) Prune() {
	if r == nil {
		return
	}
	for k, v := range r.Resources {
		if v == 0 {
			delete(r.Resources, k)
		}
	}
}

// AddTo adds the resource to the base updating the base resource
// Should be used by temporary computation only
// A nil base resource does not change
// A nil passed in resource is treated as a zero valued resource and leaves base unchanged
func (r *Resource) AddTo(add *Resource) {
	if r != nil {
		if add == nil {
			return
		}
		for k, v := range add.Resources {
			r.Resources[k] = addVal(r.Resources[k], v)
		}
	}
}

// Subtract from the resource the passed in resource by updating the resource it is called on.
// Should be used by temporary computation only
// A nil base resource does not change
// A nil passed in resource is treated as a zero valued resource and leaves the base unchanged.
func (r *Resource) SubFrom(sub *Resource) {
	if r != nil {
		if sub == nil {
			return
		}
		for k, v := range sub.Resources {
			r.Resources[k] = subVal(r.Resources[k], v)
		}
	}
}

// Multiply the resource by the ratio updating the resource it is called on.
// Should be used by temporary computation only.
func (r *Resource) MultiplyTo(ratio float64) {
	if r != nil {
		for k, v := range r.Resources {
			r.Resources[k] = mulValRatio(v, ratio)
		}
	}
}

// Calculate how well the receiver fits in "fit"
//   - A score of 0 is a fit (similar to FitIn)
//   - The score is calculated only using resource type defined in the fit resource.
//   - The score has a range between 0..#fit-res (the number of resource types in fit)
//   - Same score means same fit
//   - The lower the score the better the fit (0 is a fit)
//   - Each individual score is calculated as follows: score = (fitVal - resVal) / fitVal
//     That calculation per type is summed up for all resource types in fit.
//     example 1: fit memory 1000; resource 100; score = 0.9
//     example 2: fit memory 150; resource 15; score = 0.9
//     example 3: fit memory 100, cpu 1; resource memory 10; score = 1.9
//   - A nil receiver gives back the maximum score (number of resources types in fit)
func (r *Resource) FitInScore(fit *Resource) float64 {
	var score float64
	// short cut for a nil receiver and fit
	if r == nil || fit == nil {
		if fit != nil {
			return float64(len(fit.Resources))
		}
		return score
	}
	// walk over the defined values
	for key, fitVal := range fit.Resources {
		// negative is treated as 0 and fits always
		if fitVal <= 0 {
			continue
		}
		// negative is treated as 0 and gives max score of 1
		resVal := r.Resources[key]
		if resVal <= 0 {
			score++
			continue
		}
		// smaller values fit: score = 0 for those
		if fitVal > resVal {
			score += float64(fitVal-resVal) / float64(fitVal)
		}
	}
	return score
}

// Wrapping safe calculators for the quantities of resources.
// They will always return a valid int64. Logging if the calculator wrapped the value.
// Returning the appropriate MaxInt64 or MinInt64 value.
func addVal(valA, valB Quantity) Quantity {
	result := valA + valB
	// check if the sign wrapped
	if (result < valA) != (valB < 0) {
		if valA < 0 {
			// return the minimum possible
			log.Log(log.Resources).Warn("Resource calculation wrapped: returned minimum value possible",
				zap.Int64("valueA", int64(valA)),
				zap.Int64("valueB", int64(valB)))
			return math.MinInt64
		}
		// return the maximum possible
		log.Log(log.Resources).Warn("Resource calculation wrapped: returned maximum value possible",
			zap.Int64("valueA", int64(valA)),
			zap.Int64("valueB", int64(valB)))
		return math.MaxInt64
	}
	// not wrapped normal case
	return result
}

func subVal(valA, valB Quantity) Quantity {
	return addVal(valA, -valB)
}

func mulVal(valA, valB Quantity) Quantity {
	// optimise the zero cases (often hit with zero resource)
	if valA == 0 || valB == 0 {
		return 0
	}
	result := valA * valB
	// check the wrapping
	// MinInt64 * -1 is special: it returns MinInt64, it should return MaxInt64 but does not trigger
	// wrapping if not specially checked
	if (result/valB != valA) || (valA == math.MinInt64 && valB == -1) {
		if (valA < 0) != (valB < 0) {
			// return the minimum possible
			log.Log(log.Resources).Warn("Resource calculation wrapped: returned minimum value possible",
				zap.Int64("valueA", int64(valA)),
				zap.Int64("valueB", int64(valB)))
			return math.MinInt64
		}
		// return the maximum possible
		log.Log(log.Resources).Warn("Resource calculation wrapped: returned maximum value possible",
			zap.Int64("valueA", int64(valA)),
			zap.Int64("valueB", int64(valB)))
		return math.MaxInt64
	}
	// not wrapped normal case
	return result
}

func mulValRatio(value Quantity, ratio float64) Quantity {
	// optimise the zero cases (often hit with zero resource)
	if value == 0 || ratio == 0 {
		return 0
	}
	result := float64(value) * ratio
	// protect against positive integer overflow
	if result > math.MaxInt64 {
		log.Log(log.Resources).Warn("Multiplication result positive overflow",
			zap.Float64("value", float64(value)),
			zap.Float64("ratio", ratio))
		return math.MaxInt64
	}
	// protect against negative integer overflow
	if result < math.MinInt64 {
		log.Log(log.Resources).Warn("Multiplication result negative overflow",
			zap.Float64("value", float64(value)),
			zap.Float64("ratio", ratio))
		return math.MinInt64
	}
	// not wrapped normal case
	return Quantity(result)
}

// Operations on resources: the operations leave the passed in resources unchanged.
// Resources are sparse objects in all cases an undefined quantity is assumed zero (0).
// All operations must be nil safe.
// All operations that take more than one resource return a union of resource entries
// defined in both resources passed in. Operations must be able to handle the sparseness
// of the resource objects

// Add resources returning a new resource with the result
// A nil resource is considered an empty resource
func Add(left, right *Resource) *Resource {
	// check nil inputs and shortcut
	if left == nil {
		left = Zero
	}
	if right == nil {
		return left.Clone()
	}

	// neither are nil, clone one and add the other
	out := left.Clone()
	for k, v := range right.Resources {
		out.Resources[k] = addVal(out.Resources[k], v)
	}
	return out
}

// Subtract resource returning a new resource with the result
// A nil resource is considered an empty resource
// This might return negative values for specific quantities
func Sub(left, right *Resource) *Resource {
	// check nil inputs and shortcut
	if left == nil {
		left = Zero
	}
	if right == nil {
		return left.Clone()
	}

	// neither are nil, clone one and sub the other
	out := left.Clone()
	for k, v := range right.Resources {
		out.Resources[k] = subVal(out.Resources[k], v)
	}
	return out
}

// SubOnlyExisting subtracts delta from base resource, ignoring any type not defined in the base resource.
func SubOnlyExisting(base, delta *Resource) *Resource {
	// check nil inputs and shortcut
	if base == nil || delta == nil {
		return base.Clone()
	}
	// neither are nil, subtract the delta
	result := NewResource()
	for k := range base.Resources {
		result.Resources[k] = subVal(base.Resources[k], delta.Resources[k])
	}
	return result
}

// AddOnlyExisting adds delta to base resource, ignoring any type not defined in the base resource.
func AddOnlyExisting(base, delta *Resource) *Resource {
	// check nil inputs and shortcut
	if base == nil || delta == nil {
		return base.Clone()
	}
	// neither are nil, add the delta
	result := NewResource()
	for k := range base.Resources {
		result.Resources[k] = addVal(base.Resources[k], delta.Resources[k])
	}
	return result
}

// SubEliminateNegative subtracts resource returning a new resource with the result
// A nil resource is considered an empty resource
// This will return 0 values for negative values
func SubEliminateNegative(left, right *Resource) *Resource {
	res, _ := subNonNegative(left, right)
	return res
}

// SubErrorNegative subtracts resource returning a new resource with the result. A nil resource is considered
// an empty resource. This will return an error if any value in the result is negative.
// The caller should at least log the error.
// The returned resource is valid and has all negative values reset to 0
func SubErrorNegative(left, right *Resource) (*Resource, error) {
	res, message := subNonNegative(left, right)
	var err error
	if message != "" {
		err = errors.New(message)
	}
	return res, err
}

// Internal subtract resource returning a new resource with the result and an error message when a
// quantity in the result was less than zero. All negative values are reset to 0.
func subNonNegative(left, right *Resource) (*Resource, string) {
	message := ""
	// check nil inputs and shortcut
	if left == nil {
		left = Zero
	}
	if right == nil {
		return left.Clone(), message
	}

	// neither are nil, clone one and sub the other
	out := left.Clone()
	for k, v := range right.Resources {
		out.Resources[k] = subVal(out.Resources[k], v)
		// make sure value is not negative
		if out.Resources[k] < 0 {
			if message == "" {
				message = "resource quantity less than zero for: " + k
			} else {
				message += ", " + k
			}
			out.Resources[k] = 0
		}
	}
	return out, message
}

// FitIn checks if smaller fits in the defined resource
// Types not defined in resource this is called against are considered 0 for Quantity
// A nil resource is treated as an empty resource (no types defined)
func (r *Resource) FitIn(smaller *Resource) bool {
	return r.fitIn(smaller, false)
}

// FitInMaxUndef checks if smaller fits in the defined resource
// Types not defined in resource this is called against are considered the maximum value for Quantity
// A nil resource is treated as an empty resource (no types defined)
func (r *Resource) FitInMaxUndef(smaller *Resource) bool {
	return r.fitIn(smaller, true)
}

// Check if smaller fits in the defined resource
// Negative values will be treated as 0
// A nil resource is treated as an empty resource, behaviour defined by skipUndef
func (r *Resource) fitIn(smaller *Resource, skipUndef bool) bool {
	if r == nil {
		r = Zero // shadows in the local function not seen by the callers.
	}
	// shortcut: a nil resource always fits because negative values are treated as 0
	// this step explicitly does not check for zero values or an empty resource that is handled by the loop
	if smaller == nil {
		return true
	}

	for k, v := range smaller.Resources {
		largerValue, ok := r.Resources[k]
		// skip if not defined (queue quota checks: undefined resources are considered max)
		if skipUndef && !ok {
			continue
		}
		largerValue = max(0, largerValue)
		if v > largerValue {
			return false
		}
	}
	return true
}

// getShareFairForDenominator attempts to computes the denominator for a queue's fair share ratio.
// Here Resources can be either guaranteed Resources or fairmax Resources.
// If the quanity is explicitly 0 or negative, we will check usage.  If usage >= 0, the share will be set to 1.0.  Otherwise, it will be set 0.0.
func getShareFairForDenominator(resourceType string, allocated Quantity, denominatorResources *Resource) (float64, bool) {
	if denominatorResources == nil {
		return 0.0, false
	}

	denominator, ok := denominatorResources.Resources[resourceType]

	switch {
	case ok && denominator <= 0:
		if allocated <= 0 {
			// explicit 0 or negative value with NO usage
			return 0.0, true
		} else {
			// explicit 0 or negative value with usage
			return 1.0, true
		}
	case denominator > 0:
		return (float64(allocated) / float64(denominator)), true
	default:
		// no denominator. ie. no guarantee or fairmax for resourceType
		return 0.0, false
	}
}

// getFairShare produces a ratio which represents it's current 'fair' share usage.
// Iterate over all of the allocated resource types.  For each, compute the ratio, ultimately returning the max ratio encountered.
// The numerator will be the allocated usage.
// If guarantees are present, they will be used for the denominator, otherwise we will fallback to the 'maxfair' capacity of the cluster.
func getFairShare(allocated, guaranteed, fair *Resource) float64 {
	if allocated == nil || len(allocated.Resources) == 0 {
		return 0.0
	}

	var maxShare float64
	for k, v := range allocated.Resources {
		var nextShare float64

		// if usage <= 0, resource has no share
		if allocated.Resources[k] < 0 {
			continue
		}

		nextShare, found := getShareFairForDenominator(k, v, guaranteed)
		if !found {
			nextShare, found = getShareFairForDenominator(k, v, fair)
		}
		if found && nextShare > maxShare {
			maxShare = nextShare
		}
	}
	return maxShare
}

// Get the share of each resource quantity when compared to the total
// resources quantity
// NOTE: shares can be negative and positive in the current assumptions
func getShares(res, total *Resource) []float64 {
	// shortcut if the passed in resource to get the share on is nil or empty (sparse)
	if res == nil || len(res.Resources) == 0 {
		return make([]float64, 0)
	}
	shares := make([]float64, len(res.Resources))
	idx := 0
	for k, v := range res.Resources {
		// no usage then there is no share (skip prevents NaN)
		// floats init to 0 in the array anyway
		if v == 0 {
			continue
		}
		// Share is usage if total is nil or zero for this resource
		// Resources are integer so we could divide by 0. Handle it specifically here,
		// similar to a nil total resource. The check is not to prevent the divide by 0 error.
		// Compare against zero total resource fails without the check and some sorters use that.
		if total == nil || total.Resources[k] == 0 {
			// negative share is logged
			if v < 0 {
				log.Log(log.Resources).Debug("usage is negative no total, share is also negative",
					zap.String("resource key", k),
					zap.Int64("resource quantity", int64(v)))
			}
			shares[idx] = float64(v)
			idx++
			continue
		}
		shares[idx] = float64(v) / float64(total.Resources[k])
		// negative share is logged
		if shares[idx] < 0 {
			log.Log(log.Resources).Debug("share set is negative",
				zap.String("resource key", k),
				zap.Int64("resource quantity", int64(v)),
				zap.Int64("total quantity", int64(total.Resources[k])))
		}
		idx++
	}

	// sort in increasing order, NaN can not be part of the list
	sort.Float64s(shares)
	return shares
}

// Calculate share for left of total and right of total.
// This returns the same value as compareShares does:
// 0 for equal shares
// 1 if the left share is larger
// -1 if the right share is larger
func CompUsageRatio(left, right, total *Resource) int {
	lshares := getShares(left, total)
	rshares := getShares(right, total)

	return compareShares(lshares, rshares)
}

// Calculate share for left of total and right of total separately.
// This returns the same value as compareShares does:
// 0 for equal shares
// 1 if the left share is larger
// -1 if the right share is larger
func CompUsageRatioSeparately(leftAllocated, leftGuaranteed, leftFairMax, rightAllocated, rightGuaranteed, rightFairMax *Resource) int {
	lshare := getFairShare(leftAllocated, leftGuaranteed, leftFairMax)
	rshare := getFairShare(rightAllocated, rightGuaranteed, rightFairMax)

	switch {
	case lshare > rshare:
		return 1
	case lshare < rshare:
		return -1
	default:
		return 0
	}
}

// Get fairness ratio calculated by:
// highest share for left resource from total divided by
// highest share for right resource from total.
// If highest share for the right resource is 0 fairness is 1
func FairnessRatio(left, right, total *Resource) float64 {
	lshares := getShares(left, total)
	rshares := getShares(right, total)

	// Get the largest value from the shares
	lshare := float64(0)
	if shareLen := len(lshares); shareLen != 0 {
		lshare = lshares[shareLen-1]
	}
	rshare := float64(0)
	if shareLen := len(rshares); shareLen != 0 {
		rshare = rshares[shareLen-1]
	}
	// calculate the ratio
	ratio := lshare / rshare
	// divide by zero gives special NaN back change it to 1
	if math.IsNaN(ratio) {
		return 1
	}
	return ratio
}

// Compare the shares and return the compared value
// 0 for equal shares
// 1 if the left share is larger
// -1 if the right share is larger
func compareShares(lshares, rshares []float64) int {
	// get the length of the shares: a nil or empty share list gives -1
	lIdx := len(lshares) - 1
	rIdx := len(rshares) - 1
	// if both lists have at least 1 share start comparing
	for rIdx >= 0 && lIdx >= 0 {
		if lshares[lIdx] > rshares[rIdx] {
			return 1
		}
		if lshares[lIdx] < rshares[rIdx] {
			return -1
		}
		lIdx--
		rIdx--
	}
	// we got to the end: one of the two indexes must be negative or both are
	// case 1: nothing left on either side all shares are equal
	if lIdx == -1 && rIdx == -1 {
		return 0
	}
	// case 2: values left for the left shares
	if lIdx >= 0 {
		for lIdx >= 0 {
			if lshares[lIdx] > 0 {
				return 1
			}
			if lshares[lIdx] < 0 {
				return -1
			}
			lIdx--
		}
	}
	// case 3: values left for the right shares
	for rIdx >= 0 {
		if rshares[rIdx] > 0 {
			return -1
		}
		if rshares[rIdx] < 0 {
			return 1
		}
		rIdx--
	}
	// all left over values were 0 still equal (sparse resources)
	return 0
}

// Equals Compare the resources based on common resource type available in both left and right Resource
// Resource type available in left Resource but not in right Resource and vice versa is not taken into account
// False in case anyone of the resources is nil
// False in case resource type value differs
// True in case when resource type values of left Resource matches with right Resource if resource type is available
func Equals(left, right *Resource) bool {
	if left == right {
		return true
	}

	if left == nil || right == nil {
		return false
	}

	for k, v := range left.Resources {
		if right.Resources[k] != v {
			return false
		}
	}
	for k, v := range right.Resources {
		if left.Resources[k] != v {
			return false
		}
	}
	return true
}

// DeepEquals Compare the resources based on resource type existence and its values as well
// False in case anyone of the resources is nil
// False in case resource length differs
// False in case resource type existed in left Resource not exist in right Resource
// False in case resource type value differs
// True in case when all resource type and its values of left Resource matches with right Resource
func DeepEquals(left, right *Resource) bool {
	if left == right {
		return true
	}
	if left == nil || right == nil {
		return false
	}
	if len(right.Resources) != len(left.Resources) {
		return false
	}
	for k, v := range left.Resources {
		if val, ok := right.Resources[k]; ok {
			if val != v {
				return false
			}
		} else {
			return false
		}
	}
	return true
}

// MatchAny returns true if at least one type in the defined resource exists in the other resource.
// False if none of the types exist in the other resource.
// A nil resource is treated as an empty resource (no types defined) and returns false
// Values are not considered during the checks
func (r *Resource) MatchAny(other *Resource) bool {
	if r == nil || other == nil {
		return false
	}
	if r == other {
		return true
	}
	for k := range r.Resources {
		if _, ok := other.Resources[k]; ok {
			return true
		}
	}
	return false
}

// Compare the resources equal returns the specific values for following cases:
// left  right  return
// nil   nil    true
// nil   <set>  false
// nil zero res true
// <set>   nil    false
// zero res nil true
// <set> <set>  true/false  *based on the individual Quantity values
func EqualsOrEmpty(left, right *Resource) bool {
	if IsZero(left) && IsZero(right) {
		return true
	}
	return Equals(left, right)
}

// Multiply the resource by the integer ratio returning a new resource.
// Result is protected from overflow (positive and negative).
// A nil resource passed in returns a new empty resource (zero)
func Multiply(base *Resource, ratio int64) *Resource {
	ret := NewResource()
	// shortcut nil or zero input
	if base == nil || ratio == 0 {
		return ret
	}
	qRatio := Quantity(ratio)
	for k, v := range base.Resources {
		ret.Resources[k] = mulVal(v, qRatio)
	}
	return ret
}

// Multiply the resource by the floating point ratio returning a new resource.
// The result is rounded down to the nearest integer value after the multiplication.
// Result is protected from overflow (positive and negative).
// A nil resource passed in returns a new empty resource (zero)
func MultiplyBy(base *Resource, ratio float64) *Resource {
	ret := NewResource()
	if base == nil || ratio == 0 {
		return ret
	}
	for k, v := range base.Resources {
		ret.Resources[k] = mulValRatio(v, ratio)
	}
	return ret
}

// Return true if all quantities in larger > smaller
// Two resources that are equal are not considered strictly larger than each other.
func StrictlyGreaterThan(larger, smaller *Resource) bool {
	if larger == nil {
		larger = Zero
	}
	if smaller == nil {
		smaller = Zero
	}

	// keep track of the number of not equal values
	notEqual := false
	// check the larger side, track non equality
	for k, v := range larger.Resources {
		if smaller.Resources[k] > v {
			return false
		}
		if smaller.Resources[k] != v {
			notEqual = true
		}
	}

	// check the smaller side, track non equality
	for k, v := range smaller.Resources {
		if larger.Resources[k] < v {
			return false
		}
		if larger.Resources[k] != v {
			notEqual = true
		}
	}
	// at this point the resources are either equal or not
	// if they are not equal larger is strictly larger than smaller
	return notEqual
}

// Return true if all quantities in larger > smaller or if the two objects  are exactly the same.
func StrictlyGreaterThanOrEquals(larger, smaller *Resource) bool {
	if larger == nil {
		larger = Zero
	}
	if smaller == nil {
		smaller = Zero
	}

	for k, v := range larger.Resources {
		if smaller.Resources[k] > v {
			return false
		}
	}

	for k, v := range smaller.Resources {
		if larger.Resources[k] < v {
			return false
		}
	}

	return true
}

// StrictlyGreaterThanOnlyExisting returns true if all quantities for types in the defined resource are greater than
// the quantity for the same type in smaller.
// Types defined in smaller that are not in the defined resource are ignored.
// Two resources that are equal are not considered strictly larger than each other.
func (r *Resource) StrictlyGreaterThanOnlyExisting(smaller *Resource) bool {
	if r == nil {
		r = Zero
	}
	if smaller == nil {
		smaller = Zero
	}

	// keep track of the number of not equal values
	notEqual := false

	// Is larger and smaller completely disjoint?
	atleastOneResourcePresent := false
	// Is all resource in larger greater than zero?
	isAllPositiveInLarger := true

	for k, v := range r.Resources {
		// even when smaller is empty, all resource type in larger should be greater than zero
		if smaller.IsEmpty() && v <= 0 {
			isAllPositiveInLarger = false
		}
		// when smaller is not empty
		if val, ok := smaller.Resources[k]; ok {
			// at least one common resource type is there
			atleastOneResourcePresent = true
			if val > v {
				return false
			}
			if val != v {
				notEqual = true
			}
		}
	}

	switch {
	case smaller.IsEmpty() && !r.IsEmpty():
		return isAllPositiveInLarger
	case atleastOneResourcePresent:
		return notEqual
	default:
		// larger and smaller is completely disjoint. none of the resource match.
		return !r.IsEmpty() && !smaller.IsEmpty()
	}
}

// Have at least one quantity > 0, and no quantities < 0
// A nil resource is not strictly greater than zero.
func StrictlyGreaterThanZero(larger *Resource) bool {
	if larger == nil {
		return false
	}
	greater := false
	for _, v := range larger.Resources {
		if v < 0 {
			return false
		}
		if v > 0 {
			greater = true
		}
	}
	return greater
}

// ComponentWiseMin returns a new Resource with the smallest value for each quantity in the Resources
// If either Resource passed in is nil the other Resource is returned
// If a Resource type is missing from one of the Resource, it is considered empty and the quantity from the other Resource is returned
func ComponentWiseMin(left, right *Resource) *Resource {
	out := NewResource()
	if right == nil && left == nil {
		return nil
	}
	if left == nil {
		return right.Clone()
	}
	if right == nil {
		return left.Clone()
	}
	for k, v := range left.Resources {
		if val, ok := right.Resources[k]; ok {
			out.Resources[k] = min(v, val)
		} else {
			out.Resources[k] = v
		}
	}
	for k, v := range right.Resources {
		if val, ok := left.Resources[k]; ok {
			out.Resources[k] = min(v, val)
		} else {
			out.Resources[k] = v
		}
	}
	return out
}

// MergeIfNotPresent Returns a new Resource by merging resource type values present in right with left
// only if resource type not present in left.
// If either Resource passed in is nil the other Resource is returned
// If a Resource type is missing from one of the Resource, it is considered empty and the quantity from the other Resource is returned
func MergeIfNotPresent(left, right *Resource) *Resource {
	if right == nil && left == nil {
		return nil
	}
	if left == nil {
		return right.Clone()
	}
	if right == nil {
		return left.Clone()
	}
	out := left.Clone()
	for k, v := range right.Resources {
		if _, ok := left.Resources[k]; !ok {
			out.Resources[k] = v
		}
	}
	return out
}

// ComponentWiseMinOnlyExisting Returns a new Resource with the smallest value for resource type
// existing only in left but not vice versa.
func ComponentWiseMinOnlyExisting(left, right *Resource) *Resource {
	out := NewResource()
	if right == nil && left == nil {
		return nil
	}
	if left == nil {
		return nil
	}
	if right == nil {
		return left.Clone()
	}
	for k, v := range left.Resources {
		if val, ok := right.Resources[k]; ok {
			out.Resources[k] = min(v, val)
		} else {
			out.Resources[k] = v
		}
	}
	return out
}

func (r *Resource) HasNegativeValue() bool {
	if r == nil {
		return false
	}
	for _, v := range r.Resources {
		if v < 0 {
			return true
		}
	}
	return false
}

// IsEmpty returns true if the resource is nil or has no component resources specified.
func (r *Resource) IsEmpty() bool {
	return r == nil || len(r.Resources) == 0
}

// Returns a new resource with the largest value for each quantity in the resources
// If either resource passed in is nil a zero resource is returned
func ComponentWiseMax(left, right *Resource) *Resource {
	out := NewResource()
	if left != nil && right != nil {
		for k, v := range left.Resources {
			out.Resources[k] = max(v, right.Resources[k])
		}
		for k, v := range right.Resources {
			out.Resources[k] = max(v, left.Resources[k])
		}
	}
	return out
}

// Check that the whole resource is zero
// A nil or empty resource is zero (contrary to StrictlyGreaterThanZero)
func IsZero(zero *Resource) bool {
	if zero == nil {
		return true
	}
	for _, v := range zero.Resources {
		if v != 0 {
			return false
		}
	}
	return true
}

// CalculateAbsUsedCapacity returns absolute used as a percentage, a positive integer value, for each defined resource
// named in the capacity comparing usage to the capacity.
// If usage is 0 or below 0, absolute used is always 0
// if capacity is 0 or below 0, absolute used is always 100
// if used is larger than capacity a value larger than 100 can be returned. The percentage value returned is capped at
// math.MaxInt32 (resolved value 2147483647)
func CalculateAbsUsedCapacity(capacity, used *Resource) *Resource {
	absResource := NewResource()
	if capacity == nil || used == nil {
		log.Log(log.Resources).Debug("Cannot calculate absolute capacity because of missing capacity or usage")
		return absResource
	}
	missingResources := &strings.Builder{}
	for resourceName, capResource := range capacity.Resources {
		var absResValue int64
		usedResource, ok := used.Resources[resourceName]
		// track this for troubleshooting only
		if !ok {
			if missingResources.Len() != 0 {
				missingResources.WriteString(", ")
			}
			missingResources.WriteString(resourceName)
			continue
		}
		switch {
		// used is 0 or below nothing is used -> 0%
		// below 0 should never happen
		case usedResource <= 0:
			absResValue = 0
		// capacity is 0 or below any usage is full -> 100% (prevents divide by 0)
		// below 0 should never happen
		case capResource <= 0:
			absResValue = 100
		// calculate percentage: never wraps, could overflow int64 due to percentage conversion ONLY
		default:
			div := (float64(usedResource) / float64(capResource)) * 100
			// we really do not want to show a percentage value that is larger than a 32-bit integer.
			// even that is already really large and could easily lead to UI render issues.
			if div > float64(math.MaxInt32) {
				absResValue = math.MaxInt32
			} else {
				absResValue = int64(div)
			}
		}
		absResource.Resources[resourceName] = Quantity(absResValue)
	}
	if missingResources.Len() != 0 {
		log.Log(log.Resources).Debug("Absolute usage result is missing resource information",
			zap.Stringer("missing resource(s)", missingResources))
	}
	return absResource
}

// DominantResourceType calculates the most used resource type based on the ratio of used compared to
// the capacity. If a capacity type is set to 0 assume full usage.
// Dominant type should be calculated with queue usage and capacity. Queue capacities should never
// contain 0 values when there is a usage also, however in the root queue this could happen. If the
// last node reporting that resource was removed but not everything has been updated.
// immediately
// Ignores resources types that are used but not defined in the capacity.
func (r *Resource) DominantResourceType(capacity *Resource) string {
	if r == nil || capacity == nil {
		return ""
	}
	var div, temp float64
	dominant := ""
	for name, usedVal := range r.Resources {
		capVal, ok := capacity.Resources[name]
		if !ok {
			log.Log(log.Resources).Debug("missing resource in dominant calculation",
				zap.String("missing resource", name))
			continue
		}
		// calculate the ratio between usage and capacity
		// ratio should be somewhere between 0 and 1, but do not restrict
		// handle 0 values specifically just to be safe should never happen
		if capVal == 0 {
			if usedVal == 0 {
				temp = 0 // no usage, no cap: consider empty
			} else {
				temp = 1 // usage, no cap: fully used
			}
		} else {
			temp = float64(usedVal) / float64(capVal) // both not zero calculate ratio
		}
		// if we have exactly the same use the latest one
		if temp >= div {
			div = temp
			dominant = name
		}
	}
	return dominant
}
