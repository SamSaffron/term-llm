// go-qrcode
// Copyright 2014 Tom Harwood

package qrcode

import "slices"

// Keep upstream version order: ascending version, then Low, Medium, High, Highest.
var versions = slices.Concat(versions1To10, versions11To20, versions21To30, versions31To40)
