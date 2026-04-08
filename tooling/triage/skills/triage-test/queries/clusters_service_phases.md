# Clusters Service Phases

This query divulges state from clusters-service, a component that provisions Azure infrastructure and creates the HyperShift HostedCluster record, passes it using Maestro to the management cluster, and waits for Maestro to back-channel status. Check to see that provision steps (for Azure infra) succeeded, and that the cluster has progressed to 'installing' state. If steps fail or the cluster doesn't end up in 'installing', there's a clusters-service bug. Clusters stuck in 'installing' could either be due to a bug in Maestro signalling, or HyperShift reconciliation.
