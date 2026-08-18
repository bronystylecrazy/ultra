package main

import "hash/fnv"

// Dev ports are DERIVED FROM THE PRODUCT NAME, not defaulted. Every scaffold
// used to claim :8080 and the same e2e port, so a second product on the same
// machine could not boot beside the first — three collisions in one week.
// The derivation is a pure function of the name, so the same name always
// renders the same ports (a regenerated file is byte-identical) and two names
// practically never land on the same one.
//
// Ranges are high and documented; the port is written into config.toml, which
// is a product's own file from the moment it exists — change it freely.
const (
	httpPortLo, httpPortHi = 8100, 8999
	e2ePortLo, e2ePortHi   = 9100, 9899
	mqttPortLo, mqttPortHi = 11883, 12799
)

// reservedPorts are the ports inside those ranges that a developer machine is
// likely to have already spoken for. Landing on one is not a bug, it is a
// Monday morning spent on "why does nothing answer".
var reservedPorts = map[int]bool{
	8443: true, // https-alt
	8500: true, // consul
	8888: true, // jupyter, and half the http-alt world
	9200: true, // elasticsearch
	9229: true, // node inspector
	9300: true, // elasticsearch transport
}

// derivePort maps a product name to a stable port in [lo, hi], stepping past
// the reserved ones. salt separates the services, so a product's http port and
// its e2e port move independently of each other.
func derivePort(name, salt string, lo, hi int) int {
	h := fnv.New32a()
	h.Write([]byte(salt))
	h.Write([]byte{0})
	h.Write([]byte(name))
	span := hi - lo + 1
	p := lo + int(h.Sum32()%uint32(span))
	// Bounded: a range that is entirely reserved returns something rather
	// than spinning.
	for i := 0; i < span && reservedPorts[p]; i++ {
		p = lo + (p-lo+1)%span
	}
	return p
}

// fillPorts derives the ports this product's files carry. Idempotent — an
// explicitly set port is kept, so a caller can pin one.
func (d *scaffoldData) fillPorts() {
	if d.HTTPPort == 0 {
		d.HTTPPort = derivePort(d.Name, "http", httpPortLo, httpPortHi)
	}
	if d.E2EPort == 0 {
		d.E2EPort = derivePort(d.Name, "e2e", e2ePortLo, e2ePortHi)
	}
	if d.MQTTPort == 0 {
		d.MQTTPort = derivePort(d.Name, "mqtt", mqttPortLo, mqttPortHi)
	}
}
