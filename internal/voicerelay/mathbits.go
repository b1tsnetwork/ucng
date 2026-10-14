// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import "math"

func mathFloat64bits(f float64) uint64     { return math.Float64bits(f) }
func mathFloat64frombits(b uint64) float64 { return math.Float64frombits(b) }
