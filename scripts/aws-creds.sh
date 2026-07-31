#!/usr/bin/env bash
# Put AWS credentials where the ACK controllers can read them.
#
#   scripts/aws-creds.sh use-existing    reuse your current AWS identity (SSO or key)
#   scripts/aws-creds.sh refresh         re-issue after `aws sso login`
#   scripts/aws-creds.sh create-user     create a dedicated IAM user for this stack
#   scripts/aws-creds.sh delete-user     remove that user, its policy, and its keys
#   scripts/aws-creds.sh status          what is in the cluster, and how long it lasts
#
# The Secret is written directly to the cluster and is never a ConfigHub Unit:
# `cub variant upload` refuses to upload Secrets, and these bundles are published
# to a public registry. See docs/aws-credentials.md.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

NAMESPACE="ack-system"
SECRET_NAME="aws-creds"
USER_NAME="eks-inference-ack"
POLICY_NAME="ack-controllers"
POLICY_FILE="iam/ack-controllers-policy.json"
PROFILE=""
CLUSTER=""
KUBECONFIG_PATH=""
ASSUME_YES=0

die() {
	echo "ERROR: $*" >&2
	exit 1
}
note() { echo "  $*"; }

usage() {
	sed -n '2,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	cat <<'EOF'

Options:
  --profile NAME      AWS profile to use (default: current environment)
  --cluster NAME      cub cluster name; resolves ~/.confighub/clusters/NAME.kubeconfig
  --kubeconfig PATH   explicit kubeconfig (overrides --cluster)
  --user-name NAME    IAM user name (default: eks-inference-ack)
  --yes               do not prompt before mutating AWS
EOF
}

# ---------------------------------------------------------------- args

cmd="${1:-}"
[[ -n "$cmd" ]] || {
	usage
	exit 1
}
shift || true

while [[ $# -gt 0 ]]; do
	case "$1" in
	--profile) PROFILE="${2:?}"; shift 2 ;;
	--cluster) CLUSTER="${2:?}"; shift 2 ;;
	--kubeconfig) KUBECONFIG_PATH="${2:?}"; shift 2 ;;
	--user-name) USER_NAME="${2:?}"; shift 2 ;;
	--yes | -y) ASSUME_YES=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1" ;;
	esac
done

aws_cli() {
	if [[ -n "$PROFILE" ]]; then aws --profile "$PROFILE" "$@"; else aws "$@"; fi
}

# `cub cluster up` writes its own kubeconfig rather than adding a context to the
# default one, so `kubectl --context kind-<name>` does not work. Resolve it here;
# this is a papercut worth absorbing rather than documenting.
resolve_kubeconfig() {
	if [[ -n "$KUBECONFIG_PATH" ]]; then
		[[ -f "$KUBECONFIG_PATH" ]] || die "no kubeconfig at $KUBECONFIG_PATH"
		echo "$KUBECONFIG_PATH"
		return
	fi
	if [[ -n "$CLUSTER" ]]; then
		local p="${HOME}/.confighub/clusters/${CLUSTER}.kubeconfig"
		[[ -f "$p" ]] || die "no kubeconfig for cluster '${CLUSTER}' at $p"
		echo "$p"
		return
	fi
	local found
	found="$(find "${HOME}/.confighub/clusters" -maxdepth 1 -name '*.kubeconfig' 2>/dev/null | sort)"
	local count
	count="$(echo -n "$found" | grep -c . || true)"
	if [[ "$count" == "1" ]]; then
		echo "$found"
		return
	elif [[ "$count" -gt 1 ]]; then
		echo "Multiple cub clusters found; pass --cluster NAME:" >&2
		echo "$found" | sed 's|.*/||; s|\.kubeconfig$||; s|^|  |' >&2
		exit 1
	fi
	[[ -n "${KUBECONFIG:-}" ]] || die "no cub cluster kubeconfig found; pass --kubeconfig"
	echo "$KUBECONFIG"
}

kc() { KUBECONFIG="$(resolve_kubeconfig)" kubectl "$@"; }

confirm() {
	[[ "$ASSUME_YES" == "1" ]] && return 0
	local reply
	read -r -p "$1 [y/N] " reply
	[[ "$reply" == "y" || "$reply" == "Y" ]]
}

# The region ACK operates in comes from the chart values, and the subnet AZs in
# network.yaml must sit inside it. Read it rather than accept a second source of
# truth that can silently disagree.
stack_region() {
	yq -r '.aws.region' src/ack-controllers/values/ec2.yaml
}

# ISO8601 -> epoch seconds, on both GNU date and BSD/macOS date. Empty on failure.
epoch_of() {
	local ts="${1:-}"
	[[ -n "$ts" ]] || return 0
	date -d "$ts" +%s 2>/dev/null && return 0
	# BSD date needs an explicit format and cannot parse a numeric offset, so
	# normalise "+00:00"/"Z" to UTC first.
	local norm="${ts%%+*}"
	norm="${norm%Z}"
	norm="${norm%%.*}"
	TZ=UTC date -j -f '%Y-%m-%dT%H:%M:%S' "$norm" +%s 2>/dev/null || true
}

human_remaining() {
	local secs="${1:-}"
	[[ -n "$secs" ]] || return 0
	if ((secs <= 0)); then
		echo "EXPIRED"
	elif ((secs < 3600)); then
		echo "$((secs / 60))m"
	else
		echo "$((secs / 3600))h $(((secs % 3600) / 60))m"
	fi
}

# ---------------------------------------------------------------- secret

write_secret() {
	local key_id="$1" secret_key="$2" session_token="${3:-}" expires_at="${4:-}"
	local body

	body="[default]
aws_access_key_id = ${key_id}
aws_secret_access_key = ${secret_key}"
	if [[ -n "$session_token" ]]; then
		body="${body}
aws_session_token = ${session_token}"
	fi

	kc create namespace "$NAMESPACE" --dry-run=client -o yaml | kc apply -f - >/dev/null

	# --dry-run | apply so re-running rotates in place instead of failing.
	kc create secret generic "$SECRET_NAME" \
		--namespace "$NAMESPACE" \
		--from-literal=credentials="$body" \
		--dry-run=client -o yaml | kc apply -f - >/dev/null

	# Record the expiry as an annotation (not sensitive) so `status` can answer
	# "will these outlast the thing I am about to provision?" without re-deriving
	# it from the caller's environment, which may have moved on since.
	if [[ -n "$expires_at" ]]; then
		kc annotate secret "$SECRET_NAME" -n "$NAMESPACE" --overwrite \
			"eks-inference.confighub.com/expires-at=${expires_at}" >/dev/null
	else
		kc annotate secret "$SECRET_NAME" -n "$NAMESPACE" \
			"eks-inference.confighub.com/expires-at-" >/dev/null 2>&1 || true
	fi

	note "wrote Secret ${NAMESPACE}/${SECRET_NAME}"
}

restart_controllers() {
	if kc get deployment -n "$NAMESPACE" >/dev/null 2>&1; then
		# Credentials are read at startup, so an already-running controller will
		# not pick up a rotated key on its own.
		kc rollout restart deployment -n "$NAMESPACE" >/dev/null 2>&1 || true
		note "restarted controllers in ${NAMESPACE}"
	fi
}

# ---------------------------------------------------------------- commands

# Reuse whatever AWS identity the caller already has and is happy to point at this
# project: a permanent key in ~/.aws/credentials, or an SSO / assumed-role session.
#
# Both are supported. An expiring session is not dangerous here — when the
# credentials lapse the controllers get auth errors, ACK marks the resources
# ACK.Recoverable, and reconciliation pauses. Nothing is deleted and nothing is
# half-destroyed; `refresh` resumes it. So rather than refuse, this reports how
# long the session has left and only prompts when that is short relative to the
# ~25 minutes the stack takes to provision.
#
# Root credentials ARE refused: they are permanent, so they would otherwise pass,
# but they cannot be scoped or revoked independently.
cmd_use_existing() {
	command -v aws >/dev/null || die "aws CLI not found"

	echo "==> resolving AWS credentials${PROFILE:+ from profile '${PROFILE}'}"

	local creds key_id secret_key token expiry
	if creds="$(aws_cli configure export-credentials --format process 2>/dev/null)"; then
		key_id="$(jq -r '.AccessKeyId' <<<"$creds")"
		secret_key="$(jq -r '.SecretAccessKey' <<<"$creds")"
		token="$(jq -r '.SessionToken // empty' <<<"$creds")"
		expiry="$(jq -r '.Expiration // empty' <<<"$creds")"
	else
		# export-credentials needs AWS CLI >= 2.9. Fall back to reading the static
		# key straight out of the config, which is exactly this mode's premise.
		key_id="$(aws_cli configure get aws_access_key_id 2>/dev/null || true)"
		secret_key="$(aws_cli configure get aws_secret_access_key 2>/dev/null || true)"
		token="$(aws_cli configure get aws_session_token 2>/dev/null || true)"
		expiry=""
		[[ -n "$key_id" && -n "$secret_key" ]] ||
			die "no credentials found. Configure a profile with a permanent access key, or pass --profile."
	fi

	local ident arn
	ident="$(aws_cli sts get-caller-identity --output json)" || die "credentials are not valid"
	arn="$(jq -r .Arn <<<"$ident")"
	note "account:    $(jq -r .Account <<<"$ident")"
	note "arn:        ${arn}"
	note "access key: ${key_id}"

	# Root access keys are permanent, so they pass the checks below — but they
	# should never be used for anything, let alone handed to a controller pod.
	# Reject them on their own terms rather than with a misleading reason.
	if [[ "$arn" == *":iam::"*":root" ]]; then
		echo
		echo "  These are ROOT account credentials."
		echo
		echo "  Root access keys cannot be scoped and cannot be revoked without"
		echo "  disrupting the whole account. Do not mount them in a controller pod."
		echo "  Use '$0 create-user' to create a scoped user instead."
		exit 1
	fi

	# Two independent signals that these are not a permanent user key: a session
	# token, and an STS (rather than IAM user) ARN. Either one disqualifies.
	local temporary=0 why=""
	if [[ -n "$token" ]]; then
		temporary=1
		why="a session token is present${expiry:+, expiring ${expiry}}"
	elif [[ "$arn" != *":iam::"*":user/"* ]]; then
		temporary=1
		why="the caller is not an IAM user (assumed role or federated identity)"
	fi

	if [[ "$temporary" == "1" ]]; then
		note "type:       temporary (${why})"
		local exp_epoch remaining
		exp_epoch="$(epoch_of "$expiry")"
		if [[ -n "$exp_epoch" ]]; then
			remaining=$((exp_epoch - $(date +%s)))
			note "expires in: $(human_remaining "$remaining")"
			# The full stack takes ~25 minutes: ~5 for the network, ~15 for the EKS
			# control plane, ~5 for the nodegroup. Below that the session will very
			# likely lapse partway, which is recoverable but needs a refresh.
			if ((remaining < 1800)); then
				echo
				echo "  This session is likely to lapse before the stack finishes"
				echo "  provisioning (~25 min). That is recoverable — reconciliation"
				echo "  pauses and resumes, nothing is lost — but you will need to run:"
				echo
				echo "    aws sso login${PROFILE:+ --profile ${PROFILE}} && $0 refresh"
				echo
				confirm "  Continue anyway?" || exit 1
			fi
		fi
	else
		note "type:       permanent IAM user key"
	fi

	check_region
	write_secret "$key_id" "$secret_key" "$token" "$expiry"
	restart_controllers
	echo
	echo "Done. Verify with: $0 status"
	[[ "$temporary" == "1" ]] && echo "When the session lapses: aws sso login && $0 refresh"
	return 0
}

# Re-issue credentials from the caller's current identity and restart the
# controllers. This is the SSO loop: `aws sso login` refreshes your local session,
# this pushes it into the cluster.
#
# It exists as its own verb because the SDK reads the credentials file once at
# startup — updating the Secret alone changes the mounted file but not the
# running controller, so the restart is not optional.
cmd_refresh() {
	command -v aws >/dev/null || die "aws CLI not found"

	kc get secret "$SECRET_NAME" -n "$NAMESPACE" >/dev/null 2>&1 ||
		die "no existing Secret to refresh — run '$0 use-existing' first"

	echo "==> re-issuing credentials${PROFILE:+ from profile '${PROFILE}'}"
	local creds
	creds="$(aws_cli configure export-credentials --format process 2>/dev/null)" ||
		die "could not export credentials. Has your SSO session expired? Try: aws sso login${PROFILE:+ --profile ${PROFILE}}"

	local key_id secret_key token expiry
	key_id="$(jq -r '.AccessKeyId' <<<"$creds")"
	secret_key="$(jq -r '.SecretAccessKey' <<<"$creds")"
	token="$(jq -r '.SessionToken // empty' <<<"$creds")"
	expiry="$(jq -r '.Expiration // empty' <<<"$creds")"

	aws_cli sts get-caller-identity >/dev/null 2>&1 ||
		die "credentials are not valid. Try: aws sso login${PROFILE:+ --profile ${PROFILE}}"

	note "access key: ${key_id}"
	if [[ -n "$expiry" ]]; then
		local exp_epoch
		exp_epoch="$(epoch_of "$expiry")"
		[[ -n "$exp_epoch" ]] && note "expires in: $(human_remaining $((exp_epoch - $(date +%s))))"
	fi

	write_secret "$key_id" "$secret_key" "$token" "$expiry"
	restart_controllers
	echo
	echo "Done. Reconciliation resumes as the controllers come back up."
}

cmd_create_user() {
	command -v aws >/dev/null || die "aws CLI not found"
	[[ -f "$POLICY_FILE" ]] || die "missing $POLICY_FILE"

	local ident
	ident="$(aws_cli sts get-caller-identity --output json)" ||
		die "no valid AWS credentials to create the user with"

	echo "==> will create in account $(jq -r .Account <<<"$ident")"
	note "IAM user:     ${USER_NAME}"
	note "inline policy: ${POLICY_NAME}  (from ${POLICY_FILE})"
	note "then write an access key into Secret ${NAMESPACE}/${SECRET_NAME}"
	echo
	confirm "Proceed?" || exit 1

	if aws_cli iam get-user --user-name "$USER_NAME" >/dev/null 2>&1; then
		note "user ${USER_NAME} already exists; reusing it"
	else
		aws_cli iam create-user --user-name "$USER_NAME" \
			--tags Key=eks-inference.confighub.com/stack,Value=inference-demo >/dev/null
		note "created user ${USER_NAME}"
	fi

	aws_cli iam put-user-policy \
		--user-name "$USER_NAME" \
		--policy-name "$POLICY_NAME" \
		--policy-document "file://${POLICY_FILE}" >/dev/null
	note "attached policy ${POLICY_NAME}"

	# More than two access keys per user is an API error, and stale keys from a
	# previous run are the usual cause.
	local existing
	existing="$(aws_cli iam list-access-keys --user-name "$USER_NAME" \
		--query 'AccessKeyMetadata[].AccessKeyId' --output text)"
	if [[ -n "$existing" ]]; then
		note "removing ${existing} (superseded)"
		for k in $existing; do
			aws_cli iam delete-access-key --user-name "$USER_NAME" --access-key-id "$k"
		done
	fi

	local newkey key_id secret_key
	newkey="$(aws_cli iam create-access-key --user-name "$USER_NAME" --output json)"
	key_id="$(jq -r '.AccessKey.AccessKeyId' <<<"$newkey")"
	secret_key="$(jq -r '.AccessKey.SecretAccessKey' <<<"$newkey")"
	note "created access key ${key_id}"

	# IAM is eventually consistent: a brand new key is routinely rejected for the
	# first several seconds. Fail only if it never becomes usable.
	echo "==> waiting for the key to become usable"
	local ok=0
	for _ in $(seq 1 12); do
		if AWS_ACCESS_KEY_ID="$key_id" AWS_SECRET_ACCESS_KEY="$secret_key" \
			aws sts get-caller-identity >/dev/null 2>&1; then
			ok=1
			break
		fi
		sleep 5
	done
	[[ "$ok" == "1" ]] || die "key ${key_id} never became usable (IAM propagation)"
	note "key is live"

	check_region
	write_secret "$key_id" "$secret_key" ""
	restart_controllers
	echo
	echo "Done. Verify with: $0 status"
	echo "Remove it later with: $0 delete-user"
}

cmd_delete_user() {
	command -v aws >/dev/null || die "aws CLI not found"

	aws_cli iam get-user --user-name "$USER_NAME" >/dev/null 2>&1 ||
		die "no IAM user named ${USER_NAME}"

	echo "==> will delete IAM user ${USER_NAME}, its inline policy, and all its access keys"
	echo "    (this does NOT delete any AWS resources the stack created — see docs/teardown.md)"
	echo
	confirm "Proceed?" || exit 1

	local keys
	keys="$(aws_cli iam list-access-keys --user-name "$USER_NAME" \
		--query 'AccessKeyMetadata[].AccessKeyId' --output text)"
	for k in $keys; do
		aws_cli iam delete-access-key --user-name "$USER_NAME" --access-key-id "$k"
		note "deleted key $k"
	done

	aws_cli iam delete-user-policy --user-name "$USER_NAME" --policy-name "$POLICY_NAME" 2>/dev/null || true
	aws_cli iam delete-user --user-name "$USER_NAME"
	note "deleted user ${USER_NAME}"

	if kc get secret "$SECRET_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
		kc delete secret "$SECRET_NAME" -n "$NAMESPACE" >/dev/null
		note "deleted Secret ${NAMESPACE}/${SECRET_NAME}"
	fi
}

# The credentials themselves are region-independent, but ACK operates in the
# region from the chart values and the subnet AZs must sit inside it. A caller
# whose default region differs is usually about to be confused.
check_region() {
	local want have
	want="$(stack_region)"
	have="$(aws_cli configure get region 2>/dev/null || true)"
	if [[ -n "$have" && "$have" != "$want" ]]; then
		echo
		echo "  NOTE: your AWS default region is '${have}' but this stack provisions"
		echo "  into '${want}' (src/ack-controllers/values/*.yaml, and the AZs in"
		echo "  src/aws-network/network.yaml). The controllers use '${want}'."
	fi
}

cmd_status() {
	echo "==> cluster"
	note "kubeconfig: $(resolve_kubeconfig)"

	if ! kc get secret "$SECRET_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
		echo "  Secret ${NAMESPACE}/${SECRET_NAME}: MISSING"
		echo
		echo "  Run one of:"
		echo "    $0 use-existing     reuse your current AWS identity"
		echo "    $0 create-user      create a dedicated IAM user"
		return 0
	fi

	local key_id has_token
	key_id="$(kc get secret "$SECRET_NAME" -n "$NAMESPACE" -o jsonpath='{.data.credentials}' |
		base64 -d | awk -F' = ' '/aws_access_key_id/ {print $2}')"
	has_token="$(kc get secret "$SECRET_NAME" -n "$NAMESPACE" -o jsonpath='{.data.credentials}' |
		base64 -d | grep -c aws_session_token || true)"

	note "Secret ${NAMESPACE}/${SECRET_NAME}: present"
	note "access key: ${key_id}"

	if [[ "$has_token" == "0" ]]; then
		note "type:       long-lived access key"
	else
		local expires_at exp_epoch remaining
		expires_at="$(kc get secret "$SECRET_NAME" -n "$NAMESPACE" \
			-o jsonpath='{.metadata.annotations.eks-inference\.confighub\.com/expires-at}' 2>/dev/null || true)"
		note "type:       temporary session"
		if [[ -n "$expires_at" ]]; then
			exp_epoch="$(epoch_of "$expires_at")"
			if [[ -n "$exp_epoch" ]]; then
				remaining=$((exp_epoch - $(date +%s)))
				note "expires in: $(human_remaining "$remaining")"
				if ((remaining <= 0)); then
					echo
					echo "  Credentials have EXPIRED. The controllers are still running but"
					echo "  cannot reach AWS; ACK resources will show ACK.Recoverable and"
					echo "  reconciliation is paused. Nothing has been lost. To resume:"
					echo
					echo "    aws sso login && $0 refresh"
				fi
			fi
		fi
	fi

	echo
	echo "==> controllers"
	kc get pods -n "$NAMESPACE" --no-headers 2>/dev/null | sed 's/^/  /' || note "none running"

	echo
	echo "==> ACK resources"
	kc get vpc,subnet,natgateway,cluster,nodegroup -n aws-inference --no-headers 2>/dev/null |
		sed 's/^/  /' || note "none yet"
}

case "$cmd" in
use-existing) cmd_use_existing ;;
refresh) cmd_refresh ;;
create-user) cmd_create_user ;;
delete-user) cmd_delete_user ;;
status) cmd_status ;;
-h | --help | help) usage ;;
*) die "unknown command: ${cmd} (try --help)" ;;
esac
