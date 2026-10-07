// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import "errors"

// ErrNoObject is the error RequireObject returns for a success answer that
// holds no object.
var ErrNoObject = errors.New("the API answered with a success status but without the object")

// RequireObject returns what a gophercloud Extract call returned, with
// ErrNoObject in place of a nil object that came with no error. Extract
// decodes a 2xx answer whose body lacks the object's key, or is JSON null, to
// a nil object and no error. Wrap the call as
// RequireObject(groups.Create(ctx, client, opts).Extract()).
func RequireObject[T any](obj *T, err error) (*T, error) {
	if err == nil && obj == nil {
		return nil, ErrNoObject
	}
	return obj, err
}
