// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/inconshreveable/log15"
	tty "github.com/mattn/go-tty"
	"github.com/pborman/uuid"
	"github.com/vechain/thor/api"
	"github.com/vechain/thor/cmd/thor/node"
	"github.com/vechain/thor/cmd/thor/solo"
	"github.com/vechain/thor/logdb"
	"github.com/vechain/thor/lvldb"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/txpool"
	cli "gopkg.in/urfave/cli.v1"
)

var (
	version   string
	gitCommit string
	gitTag    string
	log       = log15.New()
)

func fullVersion() string {
	versionMeta := "release"
	if gitTag == "" {
		versionMeta = "dev"
	}
	return fmt.Sprintf("%s-%s-%s", version, gitCommit, versionMeta)
}

func main() {
	app := cli.App{
		Version:   fullVersion(),
		Name:      "Thor",
		Usage:     "Node of VeChain Thor Network",
		Copyright: "2018 VeChain Foundation <https://vechain.org/>",
		Flags: []cli.Flag{
			networkFlag,
			configDirFlag,
			dataDirFlag,
			beneficiaryFlag,
			apiAddrFlag,
			apiCorsFlag,
			verbosityFlag,
			maxPeersFlag,
			p2pPortFlag,
			natFlag,
		},
		Action: defaultAction,
		Commands: []cli.Command{
			{
				Name:  "solo",
				Usage: "client runs in solo mode for test & dev",
				Flags: []cli.Flag{
					dataDirFlag,
					apiAddrFlag,
					apiCorsFlag,
					onDemandFlag,
					persistFlag,
					verbosityFlag,
				},
				Action: soloAction,
			},
			{
				Name:  "master-key",
				Usage: "import and export master key",
				Flags: []cli.Flag{
					configDirFlag,
					importMasterKeyFlag,
					exportMasterKeyFlag,
				},
				Action: masterKeyAction,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func defaultAction(ctx *cli.Context) error {
	defer func() { log.Info("exited") }()

	initLogger(ctx)
	gene := selectGenesis(ctx)
	instanceDir := makeInstanceDir(ctx, gene)

	mainDB := openMainDB(ctx, instanceDir)
	defer func() { log.Info("closing main database..."); mainDB.Close() }()

	logDB := openLogDB(ctx, instanceDir)
	defer func() { log.Info("closing log database..."); logDB.Close() }()

	chain := initChain(gene, mainDB, logDB)
	master := loadNodeMaster(ctx)

	txPool := txpool.New(chain, state.NewCreator(mainDB))
	defer func() { log.Info("closing tx pool..."); txPool.Close() }()

	p2pcom := startP2PComm(ctx, chain, txPool, instanceDir)
	defer p2pcom.Shutdown()

	apiSrv, apiURL := startAPIServer(ctx, api.New(chain, state.NewCreator(mainDB), txPool, logDB, p2pcom.comm))
	defer func() { log.Info("stopping API server..."); apiSrv.Shutdown(context.Background()) }()

	printStartupMessage(gene, chain, master, instanceDir, apiURL)

	return node.New(master, chain, state.NewCreator(mainDB), logDB, txPool, p2pcom.comm).
		Run(handleExitSignal())
}

func soloAction(ctx *cli.Context) error {
	defer func() { log.Info("exited") }()

	initLogger(ctx)
	gene := soloGenesis(ctx)

	var mainDB *lvldb.LevelDB
	var logDB *logdb.LogDB
	var instanceDir string

	if ctx.Bool("persist") {
		instanceDir = makeInstanceDir(ctx, gene)
		mainDB = openMainDB(ctx, instanceDir)
		logDB = openLogDB(ctx, instanceDir)
	} else {
		instanceDir = "Memory"
		mainDB = openMemMainDB()
		logDB = openMemLogDB()
	}

	defer func() { log.Info("closing main database..."); mainDB.Close() }()
	defer func() { log.Info("closing log database..."); logDB.Close() }()

	chain := initChain(gene, mainDB, logDB)

	txPool := txpool.New(chain, state.NewCreator(mainDB))
	defer func() { log.Info("closing tx pool..."); txPool.Close() }()

	soloContext := solo.New(chain, state.NewCreator(mainDB), logDB, txPool, ctx.Bool("on-demand"))

	apiSrv, apiURL := startAPIServer(ctx, api.New(chain, state.NewCreator(mainDB), txPool, logDB, solo.Communicator{}))
	defer func() { log.Info("stopping API server..."); apiSrv.Shutdown(context.Background()) }()

	printSoloStartupMessage(gene, chain, instanceDir, apiURL)

	return soloContext.Run(handleExitSignal())
}

func masterKeyAction(ctx *cli.Context) error {
	hasImportFlag := ctx.Bool(importMasterKeyFlag.Name)
	hasExportFlag := ctx.Bool(exportMasterKeyFlag.Name)
	if hasImportFlag && hasExportFlag {
		return fmt.Errorf("flag %s and %s are exclusive", importMasterKeyFlag.Name, exportMasterKeyFlag.Name)
	}

	if !hasImportFlag && !hasExportFlag {
		return fmt.Errorf("missing flag, either %s or %s", importMasterKeyFlag.Name, exportMasterKeyFlag.Name)
	}

	configDir := makeConfigDir(ctx)
	if hasImportFlag {
		var keyjson string
		for {
			if _, err := fmt.Fscanln(os.Stdin, &keyjson); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}

		t, err := tty.Open()
		if err != nil {
			return err
		}
		defer t.Close()

		fmt.Printf("Enter passphrase:")
		passwd, err := t.ReadPassword()
		if err != nil {
			return err
		}

		key, err := keystore.DecryptKey([]byte(keyjson), passwd)
		if err != nil {
			return err
		}

		return crypto.SaveECDSA(filepath.Join(configDir, "master.key"), key.PrivateKey)
	}

	if hasExportFlag {
		masterKey, err := loadOrGeneratePrivateKey(filepath.Join(configDir, "master.key"))
		if err != nil {
			return err
		}

		t, err := tty.Open()
		if err != nil {
			return err
		}
		defer t.Close()

		fmt.Printf("Enter passphrase: ")
		passwd, err := t.ReadPassword()

		if err != nil {
			return err
		}

		keyjson, err := keystore.EncryptKey(&keystore.Key{
			PrivateKey: masterKey,
			Address:    crypto.PubkeyToAddress(masterKey.PublicKey),
			Id:         uuid.NewRandom()},
			passwd, keystore.StandardScryptN, keystore.StandardScryptP)
		if err != nil {
			return err
		}
		fmt.Println(string(keyjson))

		return nil
	}

	return nil
}
