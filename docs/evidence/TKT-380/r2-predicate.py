"""TKT-380 DISPOSABLE PROTOTYPE of ADR-084 reading R2. NOT PRODUCTION CODE.

run.sh applies this to the TEMPORARY copy of
services/inventory/internal/store/channel_allocations.go only. It changes the
liveness part of codeRedeemedQuantity, the count that redeemPresaleCode reads
under the code lock, from consumingClaims (confirmed, or live on
transaction-start now()) to every confirmed, finalizing or held claim. So an
expired held claim counts until a terminal write commits (reading R2).

The quantity expression (consumedQuantity, net of refunds) is not changed:
the R2 schedules make no refund, so the presale refund rule does not affect
them. The operator read (PresaleCodeStatuses) is not changed either.

Usage: python3 -I r2-predicate.py PATH_TO_channel_allocations.go
"""
import sys

OLD = """	WHERE organizer_id=$1 AND channel_code=$2 AND presale_code=$3 AND ` + consumingClaims
"""

NEW = """	WHERE organizer_id=$1 AND channel_code=$2 AND presale_code=$3 AND status IN ('confirmed','finalizing','held')`
"""

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    src = f.read()
if src.count(OLD) != 1:
    sys.exit("r2-predicate: the expected codeRedeemedQuantity predicate was not found exactly once")
with open(path, "w", encoding="utf-8") as f:
    f.write(src.replace(OLD, NEW))
print("r2-predicate applied")
