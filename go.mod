module github.com/coinbase/rosetta-cli

go 1.16

require (
	github.com/coinbase/rosetta-sdk-go v0.7.1
	github.com/fatih/color v1.13.0
	github.com/olekukonko/tablewriter v0.0.5
	github.com/spf13/cobra v1.2.1
	github.com/stretchr/testify v1.7.0
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
)

replace github.com/coinbase/rosetta-sdk-go v0.7.1 => github.com/millken/rosetta-sdk-go v0.7.11
