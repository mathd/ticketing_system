"""TKT-380 DISPOSABLE PROTOTYPE of the ADR-084 window rule. NOT PRODUCTION CODE.

run.sh applies this to the TEMPORARY copy of
services/inventory/internal/store/presale_codes.go only. It splits
redeemPresaleCode's single `SELECT max_redemptions, (codeWindowOpen) ... FOR
UPDATE` into the lock statement and a SEPARATE window statement that runs after
the lock is held, so clock_timestamp() is read after the lock wait.

Usage: python3 -I window-fix.py PATH_TO_presale_codes.go
"""
import sys

OLD = """	err := tx.QueryRowContext(ctx,
		`SELECT max_redemptions, (`+codeWindowOpen+`) FROM presale_codes
		 WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE`,
		org, channel, code).Scan(&maxRedemptions, &windowOpen)
"""

NEW = """	err := tx.QueryRowContext(ctx,
		`SELECT max_redemptions FROM presale_codes
		 WHERE organizer_id=$1 AND channel_code=$2 AND code=$3 FOR UPDATE`,
		org, channel, code).Scan(&maxRedemptions)
	if err == nil {
		err = tx.QueryRowContext(ctx,
			`SELECT (`+codeWindowOpen+`) FROM presale_codes
			 WHERE organizer_id=$1 AND channel_code=$2 AND code=$3`,
			org, channel, code).Scan(&windowOpen)
	}
"""

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    src = f.read()
if src.count(OLD) != 1:
    sys.exit("window-fix: the expected redeemPresaleCode statement was not found exactly once")
with open(path, "w", encoding="utf-8") as f:
    f.write(src.replace(OLD, NEW))
print("window-fix applied")
