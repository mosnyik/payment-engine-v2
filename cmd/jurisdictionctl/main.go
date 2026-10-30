// Command jurisdictionctl manages per-currency KYB document requirements
// (jurisdiction_kyb_requirements) — config-driven, ops-facing, no HTTP
// route exists for this by design (see internal/compliance/jurisdiction.go
// and docs/IMPLEMENTATION_PLAN.md's Phase 10 section), same reasoning
// adminctl already applies to admin account provisioning.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func main() {
	currency := flag.String("currency", "", "fiat currency code, e.g. NGN (required unless -list)")
	jurisdiction := flag.String("jurisdiction", "", "human-readable regulator label, e.g. \"CBN/SEC/NFIU\" (required when setting)")
	fields := flag.String("fields", "", "comma-separated required field names, e.g. cac_registration_number,tin,director_bvn (empty means no requirement)")
	list := flag.Bool("list", false, "list every currently configured jurisdiction requirement instead of setting one")
	flag.Parse()

	cfg, err := config.LoadEnv()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// Safe to call even if the server has never run yet, same as adminctl.
	if err := db.Migrate(cfg.DatabaseURL, "migrations"); err != nil {
		log.Fatal(err)
	}

	store := compliance.New(pool, compliance.NewRegistry())

	if *list {
		if err := runList(ctx, store); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *currency == "" || *jurisdiction == "" {
		fmt.Fprintln(os.Stderr, "usage: jurisdictionctl -currency=NGN -jurisdiction=\"CBN/SEC/NFIU\" -fields=cac_registration_number,tin,director_bvn")
		fmt.Fprintln(os.Stderr, "   or: jurisdictionctl -list")
		os.Exit(1)
	}

	var fieldList []string
	if strings.TrimSpace(*fields) != "" {
		for f := range strings.SplitSeq(*fields, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				fieldList = append(fieldList, f)
			}
		}
	}

	id, err := store.UpsertJurisdictionRequirement(ctx, *currency, *jurisdiction, fieldList)
	if err != nil {
		log.Fatal(err)
	}

	if len(fieldList) == 0 {
		fmt.Printf("%s (%s): no fields required (id: %s)\n", *currency, *jurisdiction, id)
		return
	}
	fmt.Printf("%s (%s): requires %s (id: %s)\n", *currency, *jurisdiction, strings.Join(fieldList, ", "), id)
}

func runList(ctx context.Context, store *compliance.Store) error {
	reqs, err := store.ListJurisdictionRequirements(ctx)
	if err != nil {
		return err
	}
	if len(reqs) == 0 {
		fmt.Println("no jurisdiction requirements configured")
		return nil
	}
	for _, r := range reqs {
		if len(r.RequiredFields) == 0 {
			fmt.Printf("%s (%s): no fields required\n", r.FiatCurrency, r.Jurisdiction)
			continue
		}
		fmt.Printf("%s (%s): %s\n", r.FiatCurrency, r.Jurisdiction, strings.Join(r.RequiredFields, ", "))
	}
	return nil
}
