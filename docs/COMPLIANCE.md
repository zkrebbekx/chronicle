# chronicle — what regulation actually requires

Most audit-log products tell you they are "HIPAA compliant" or "SOX compliant"
or provide a "tamper-proof, immutable audit trail as required by 21 CFR
Part 11". Those claims are, in the main, unsourceable to regulatory text.

This document records what the primary texts actually say, so you can decide
what you need — and so chronicle's own README does not repeat myths.

Verified 2026-07-20 against Cornell LII, govinfo authenticated CFR XML, the
eCFR versioner API, and pcaobus.org. Where a claim could not be verified, it is
marked **unverified** rather than stated.

> Not legal advice. chronicle is a library; compliance is a property of your
> whole system, your processes and your regulator's view of them.

## The short version

**No regulation surveyed here textually requires cryptographic tamper-evidence,
immutability, WORM storage, hash chaining, or Merkle trees for change records.**

21 CFR 11.10 contains no occurrence of *tamper-evident*, *hash*, *immutable* or
*WORM*. What the texts require is that audit trails **exist**, be
computer-generated and time-stamped, attribute actions to identified operators,
not obscure prior values, and be retained.

Full-row versioning satisfies the integrity requirement with no cryptography at
all.

## 21 CFR Part 11 — FDA electronic records

The strongest audit-trail mandate in the set.

**§11.10(e)**, in full:

> Use of secure, computer-generated, time-stamped audit trails to independently
> record the date and time of operator entries and actions that create, modify,
> or delete electronic records. Record changes shall not obscure previously
> recorded information. Such audit trail documentation shall be retained for a
> period at least as long as that required for the subject electronic records
> and shall be available for agency review and copying.

Note what this does *not* say. It does not require inalterable media. FDA
addressed that directly in the 1997 final-rule preamble (62 FR 13430):

> Comment 111: neither 11.70, nor other sections in part 11, requires that
> records be kept on inalterable media. What is required is that whenever
> revisions to a record are made, the original entries must not be obscured.

And on cryptography specifically, Comment 112: FDA "does not believe that
cryptographic and digital signature methods would be the only ways of linking
an electronic signature to an electronic document."

**Retention is relative**, not a fixed number of years: as long as the subject
records require.

**Attribution hooks:** §11.10(e) records "operator entries and actions";
§11.10(g) requires authority checks over who may "alter a record"; §11.10(j)
requires policies holding individuals accountable for actions under their
signatures.

**Signatures (§11.50, §11.70)** — if you use them, signed records must carry
the signer's printed name, the date and time of signing, and the *meaning* of
the signature (review, approval, responsibility, authorship), and these must
appear in any **human-readable rendering**. §11.70 requires signature-to-record
linking such that signatures cannot be excised or transferred.

> Note: §11.50(a)(3)'s "meaning" is the meaning of the **signature**, not a
> business justification for a data change. Vendors conflate the two. A claim
> that this constitutes a reason-for-change mandate was refuted 0-3.

**Unverified:** whether FDA's 2003 "Part 11: Scope and Application" guidance
places §11.10(e) audit trails under enforcement discretion while leaving
§11.10(g)/(j) and Subpart C fully enforced. Every verifier flagged this; none
could reach the document (FDA URLs 404'd). It matters, because it determines
how much weight §11.10(e) actually carries.

## HIPAA — 45 CFR Part 164

**§164.312(b) Audit controls**, in its entirety:

> Implement hardware, software, and/or procedural mechanisms that record and
> examine activity in information systems that contain or use electronic
> protected health information.

That is the whole standard. It names no technology, no format, no retention
period, and no field set.

**The six-year myth.** §164.316(b)(2)(i) requires retaining "the documentation
required by paragraph (b)(1)" for six years. Paragraph (b)(1) is written
policies and procedures, and records of actions/activities/assessments the
Security Rule requires to be documented. **Nothing in §164.316 mentions audit
logs, audit trails, system activity records, or entity data.**

> Honest caveat: a non-frivolous secondary reading holds that security audit
> logs are "record[s] of the action, activity" caught by (b)(1)(ii), since
> §164.312(b) requires recording system activity. This is contestable and
> unsettled. It is the difference between "HIPAA imposes no log retention
> period" and "HIPAA arguably imposes six years". chronicle ships no default
> retention and lets you set the period your counsel advises.

**Integrity provisions** are *Addressable*, not *Required*: §164.312(c)(2)
(corroborate ePHI has not been altered or destroyed) and §164.312(e)(2)(i)
(transmitted ePHI "not improperly modified without detection" — scoped to
network transmission only). Under §164.306(b), covered entities "may use any
security measures that allow [them] to reasonably and appropriately implement"
the standards.

## SOX lineage — PCAOB AS 1215 and SEC Rule 2-06

Seven-year retention is real and textually mandated — **and it binds the
external audit firm, not you.**

**PCAOB AS 1215 .14:**

> The auditor must retain audit documentation for seven years from the date the
> auditor grants permission to use the auditor's report in connection with the
> issuance of the company's financial statements (report release date), unless
> a longer period of time is required by law.

**17 CFR 210.2-06(a)** imposes the parallel seven years on "the accountant",
expressly including electronic records.

Neither reaches an issuer's operational database or entity-change history.

**AS 1215 .16** is the clearest append-only language in the entire corpus:

> Audit documentation must not be deleted or discarded after the documentation
> completion date, however, information may be added. Any documentation added
> must indicate the date the information was added, the name of the person who
> prepared the additional documentation, and the reason for adding it.

This is the one place in the researched texts mandating **who and why
together** — and again, it binds audit firms' workpapers after the
documentation completion date.

**Unverified, and the most important remaining gap:** obligations landing
directly on the *issuer* rather than the auditor — 18 U.S.C. §1519/§1520,
Exchange Act §17(a) and Rule 17a-4 for regulated entities. Every SOX-lineage
finding here binds the wrong party for chronicle's purposes.

## Legal hold — FRCP 37(e)

A preservation-and-sanctions standard with **zero technical content**. It
prescribes no storage format, no immutability, no hashing.

What matters for design is the trigger. Rule 37(e) applies to information
"that should have been preserved in the anticipation or conduct of
litigation". Per the 2015 Advisory Committee Note, "A variety of events may
alert a party to the prospect of litigation" — the duty attaches on
*anticipation*, judged after the fact by a court, **not** on complaint filing.

**Design consequence:** a legal hold must accept a **backdated,
operator-asserted effective timestamp**, and suspend routine and
retention-driven deletion for scoped records from that moment. A hold that can
only take effect "now" cannot express the obligation.

The severe sanctions in 37(e)(2) — adverse-inference instruction, dismissal,
default judgment — require a finding that the party "acted with the intent to
deprive another party of the information's use in the litigation."

## GDPR — not researched

**Four research sweeps failed to return a single verified claim on GDPR.**
Art.17 erasure, Art.5(1)(e) storage limitation and Art.30 records of processing
are all unresearched here.

In particular, **no DPA decision, EDPB guidance, or court ruling accepting
destruction of a per-subject encryption key as erasure under Art.17 was
verified.**

chronicle therefore describes crypto-shredding **functionally**, and hedges the
legal characterization:

> chronicle supports per-subject encryption keys. Destroying a key renders that
> subject's historical values unrecoverable while preserving the record
> structure. Whether key destruction constitutes erasure under GDPR Art.17
> depends on your supervisory authority's position; chronicle makes no
> compliance claim.

If you need a settled answer, get it from counsel, not from a library README.

## What this means for chronicle

| Feature | Justification | Strength |
|---|---|---|
| Non-destructive versioning | 21 CFR 11.10(e) "shall not obscure previously recorded information" | **Textual** |
| Required actor on every write | 11.10(e) operator entries; 11.50(a)(1); AS 1215 .16 | **Textual** |
| Computer-generated timestamps | 11.10(e) "computer-generated, time-stamped" | **Textual** |
| Backdated legal hold | FRCP 37(e) trigger is anticipation, not filing | **Textual** |
| Configurable retention, no default | Cited periods bind other parties or other objects | **Textual (negative)** |
| Optional reason field | One textual home, binding audit firms only | Weak — kept optional |
| Optional hash chaining | No regulation requires it | **None** — offered, not claimed |
| Crypto-shredding | Unverified | **Unverified** — hedged |
