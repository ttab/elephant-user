//go:build mage
// +build mage

package main

import (
	//mage:import sql
	_ "github.com/ttab/mage/sql"
	//mage:import twirp
	_ "github.com/ttab/mage/twirp"
)
