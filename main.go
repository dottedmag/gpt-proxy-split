package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/spf13/pflag"
)

func cliUsage() {
	fmt.Fprintf(os.Stderr, `Usage: gpt-proxy-split (serve|list-users|set-user-key|delete-user) <args>

gpt-proxy-split serve <listenURL>

gpt-proxy-split list-users

gpt-proxy-split set-user-key <user-name> <key>

gpt-proxy-split delete-user <user-name>

gpt-proxy-split get-usage
`)
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	pflag.Parse()

	if pflag.NArg() == 0 {
		cliUsage()
	}

	switch pflag.Arg(0) {
	case "serve":
		serveCmd(pflag.Args()[1:])
	case "list-users":
		listUsersCmd(pflag.Args()[1:])
	case "set-user-key":
		setUserKeyCmd(pflag.Args()[1:])
	case "delete-user":
		deleteUserCmd(pflag.Args()[1:])
	case "get-usage":
		getUsageCmd(pflag.Args()[1:])
	default:
		cliUsage()
	}
}

func serveCmd(args []string) {
	if len(args) != 1 {
		cliUsage()
	}

	pool := mustNewPool()
	defer pool.Close()

	serve(pool, args[0])
}

func listUsersCmd(args []string) {
	if len(args) != 0 {
		cliUsage()
	}

	pool := mustNewPool()
	defer pool.Close()

	db := mustGetDB(context.Background(), pool)
	defer pool.Put(db)

	users, err := listUsers(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list users: %v\n", err)
		os.Exit(1)
	}

	for _, user := range users {
		fmt.Printf("%s\t%s\n", user.name, user.key)
	}
}

func setUserKeyCmd(args []string) {
	if len(args) != 2 {
		cliUsage()
	}

	pool := mustNewPool()
	defer pool.Close()

	db := mustGetDB(context.Background(), pool)
	defer pool.Put(db)

	if err := setUserKey(db, args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to set user key: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("User %s is created/updated\n", args[0])
}

func deleteUserCmd(args []string) {
	if len(args) != 1 {
		cliUsage()
	}

	pool := mustNewPool()
	defer pool.Close()

	db := mustGetDB(context.Background(), pool)
	defer pool.Put(db)

	deleted, err := deleteUser(db, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to delete user: %v\n", err)
		os.Exit(1)
	}

	if deleted {
		fmt.Fprintf(os.Stderr, "User %s is deleted\n", args[0])
	} else {
		fmt.Fprintf(os.Stderr, "User %s is not found\n", args[0])
	}
}

// FIXME: split by model and calculate cost

func getUsageCmd(args []string) {
	if len(args) != 0 {
		cliUsage()
	}

	pool := mustNewPool()
	defer pool.Close()

	db := mustGetDB(context.Background(), pool)
	defer pool.Put(db)

	usage, err := getUsage(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get usage: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("User            Project           Tokens")
	fmt.Println("----------------------------------------")
	for _, monthUsage := range usage {
		fmt.Printf("%s\n----------------------------------------\n", monthUsage.month)
		for _, user := range monthUsage.projects {
			fmt.Printf("%-16s%-16s%8d\n", user.userName, user.projectName, user.tokens)
		}
	}
}
