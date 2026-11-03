package postgres

// CountApprovalsSQL отдаёт внешнему тестовому пакету текст запроса
// CountApprovals: тест плана (CLAB-371) гонит ровно его, а не копию.
const CountApprovalsSQL = countApprovalsSQL

// RunSelfBasisSQL — то же для RunSelfBasis (CLAB-380).
const RunSelfBasisSQL = runSelfBasisSQL
