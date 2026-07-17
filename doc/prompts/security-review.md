 # Task for security review

 Please review the codebase for security.
 
 The purpose of the tool is stand between external 3rd parties / the internet, and the NAS in the internal network.
 Consequently, the tool needs to be robust to protect the NAS, and the internal network.
 It does explicitly NOT need to defend against a compromised NAS. The NAS is trusted.

 Give a thorough assessment, and suggest possible remediations.
 Consider the contents of security-decisions.md for your assessment.

 Do not implement anything / do not make any changes in any files.
 First present your assessment. Then walk me through each finding one by one, so we can decide what to remediate in which way, and what to document as acceptable (by updating security-decisions.md).
 