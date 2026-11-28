//! What a real kernel printed (FLU-8 X1, X13), verbatim, for the parser's and
//! the classifier's tests. ONE place, so that a later measurement corrects it
//! here. Source: `.omc/wave-1/out/probe-nft-text-infra-saas-lab.out`, a lab VM
//! with nftables 1.0.9, kernel 6.8.0, Ubuntu 24.04, 2026-11-28; the scratch
//! tables were named `felucca_probe_w1` (renamed `felucca` in the LISTINGS
//! here, nothing else changed; the error texts are as printed). Each stderr
//! ends with the offending line and its carets. The listings mix the probe's
//! variants (terse, stateless) as labelled.

/// `nft -f -` refused by the kernel, exit status 1 (probe a).
pub(crate) const REFUSED_FROM_STDIN: &str = concat!(
    "/dev/stdin:1:13-28: Error: Could not process rule: No such file or directory\n",
    "add rule ip felucca_probe_w1 nosuch accept\n",
    "            ^^^^^^^^^^^^^^^^\n",
);

/// `nft -f -`: an `add chain` with a hook on a chain that exists without
/// one, exit status 1 (probe a3).
pub(crate) const REFUSED_HOOK_CHANGE: &str = concat!(
    "/dev/stdin:2:39-71: Error: Chain of type \"filter\" is not supported, perhaps kernel support is missing?\n",
    "add chain ip felucca_probe_w1 input { type filter hook input priority 0 ; policy accept ; }\n",
    "                                      ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^\n",
);

/// `sudo -n nft -f -` under the agent unit's limits, exit status 1 (probe a4).
pub(crate) const SUDO_UNDER_THE_UNIT: &str = "sudo: PERM_SUDOERS: setresuid(-1, 1, -1): Operation not permitted\n\
sudo: unable to open /etc/sudoers: Operation not permitted\n\
sudo: error initializing audit plugin sudoers_audit\n";

/// `nft -t list table ip <missing>` on the command line, exit status 1
/// (probe d).
pub(crate) const MISSING_TABLE: &str = "Error: No such file or directory\n\
list table ip felucca_probe_w1\n";

/// `nft -t list table ip …` (probe b, terse: the set's elements are gone).
pub(crate) const IP_TABLE_TERSE: &str = "table ip felucca {\n\
\tset tenant_pairs {\n\
\t\ttype ipv4_addr . ipv4_addr\n\
\t}\n\
\n\
\tchain forward {\n\
\t\ttype filter hook forward priority filter; policy drop;\n\
\t\tcounter packets 0 bytes 0 comment \"felucca:178b68f97cbedf3c\"\n\
\t\tiifname != \"felucca0\" accept\n\
\t\tip saddr . ip daddr @tenant_pairs accept\n\
\t}\n\
\n\
\tchain input {\n\
\t\ttype filter hook input priority filter; policy accept;\n\
\t\tcounter packets 255 bytes 35504 comment \"felucca:127f8a56fd7a7e89\"\n\
\t\tiifname \"felucca0\" ct state established,related ct direction reply accept\n\
\t}\n\
\n\
\tchain ingress {\n\
\t\ttype nat hook prerouting priority dstnat; policy accept;\n\
\t\tcounter packets 32 bytes 1920 comment \"felucca:1f9a0da00ef43aec\"\n\
\t}\n\
}\n";

/// `nft list table ip …` (probe b, not terse: with the set's elements).
pub(crate) const IP_TABLE_FULL: &str = "table ip felucca {\n\
\tset tenant_pairs {\n\
\t\ttype ipv4_addr . ipv4_addr\n\
\t\telements = { 10.231.0.3 . 10.231.0.2,\n\
\t\t\t     10.231.0.2 . 10.231.0.3 }\n\
\t}\n\
\n\
\tchain forward {\n\
\t\ttype filter hook forward priority filter; policy drop;\n\
\t\tcounter packets 0 bytes 0 comment \"felucca:178b68f97cbedf3c\"\n\
\t\tiifname != \"felucca0\" accept\n\
\t\tip saddr . ip daddr @tenant_pairs accept\n\
\t}\n\
}\n";

/// `nft list table bridge …` (probe b).
pub(crate) const BRIDGE_TABLE: &str = "table bridge felucca {\n\
\tchain forward {\n\
\t\ttype filter hook forward priority filter; policy accept;\n\
\t\tcounter packets 0 bytes 0 comment \"felucca:082beb575e48a3b3\"\n\
\t\tether type arp accept\n\
\t}\n\
}\n";

/// `nft -s -t list table netdev …` (probe b, stateless: no counter values).
pub(crate) const NETDEV_TABLE_STATELESS: &str = "table netdev felucca {\n\
\tchain as_hth_1 {\n\
\t\ttype filter hook ingress device \"lo\" priority filter; policy drop;\n\
\t\tcounter comment \"felucca:2955b13811b4c826\"\n\
\t\tether type arp accept\n\
\t\tdrop\n\
\t}\n\
\n\
\tchain as_hth_10 {\n\
\t\ttype filter hook ingress device \"lo\" priority filter; policy drop;\n\
\t\tcounter comment \"felucca:00000000000000aa\"\n\
\t\tdrop\n\
\t}\n\
}\n";

/// `nft list table netdev …` (probe b).
pub(crate) const NETDEV_TABLE: &str = "table netdev felucca {\n\
\tchain as_hth_1 {\n\
\t\ttype filter hook ingress device \"lo\" priority filter; policy drop;\n\
\t\tcounter packets 0 bytes 0 comment \"felucca:2955b13811b4c826\"\n\
\t\tether type arp accept\n\
\t\tdrop\n\
\t}\n\
\n\
\tchain as_hth_10 {\n\
\t\ttype filter hook ingress device \"lo\" priority filter; policy drop;\n\
\t\tcounter packets 87 bytes 7396 comment \"felucca:00000000000000aa\"\n\
\t\tdrop\n\
\t}\n\
}\n";

/// The head of a dormant table's listing (probe c).
pub(crate) const DORMANT_HEAD: &str = "table ip felucca {\n\
\tflags dormant\n\
\n\
\tset tenant_pairs {\n\
\t\ttype ipv4_addr . ipv4_addr\n\
\t}\n\
}\n";
