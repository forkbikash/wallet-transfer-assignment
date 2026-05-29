ByteByteGo logo

System Design Interview
0/31 completed
00
Foreword

01
Join the Community

02
Scale From Zero To Millions Of Users

03
Back-of-the-envelope Estimation

04
A Framework For System Design Interviews

05
Design A Rate Limiter

06
Design Consistent Hashing

07
Design A Key-value Store

08
Design A Unique ID Generator In Distributed Systems

09
Design A URL Shortener

10
Design A Web Crawler

11
Design A Notification System

12
Design A News Feed System

13
Design A Chat System

14
Design A Search Autocomplete System

15
Design YouTube

16
Design Google Drive

17
Proximity Service

18
Nearby Friends

19
Google Maps

20
Distributed Message Queue

21
Metrics Monitoring and Alerting System

22
Ad Click Event Aggregation

23
Hotel Reservation System

24
Distributed Email Service

25
S3-like Object Storage

26
Real-time Gaming Leaderboard

27
Payment System

28
Digital Wallet

29
Stock Exchange

30
The Learning Continues

ByteByteGo logo

28
Digital Wallet
Payment platforms usually provide a digital wallet service to clients, so they can store money in the wallet and spend it later. For example, you can add money to your digital wallet from your bank card and when you buy products online, you are given the option to pay using the money in your wallet. Figure 1 shows this process.

Image represents a simplified payment flow diagram.  It shows a 'Bank' component, depicted as a house-shaped box containing a credit card symbol and labeled 'Bank card' and 'Bank'.  An arrow labeled 'Deposit' connects the Bank to a 'Payment System' component. The Payment System is a box containing a smaller box with a dollar sign ($) representing a 'Digital wallet'.  A further arrow connects the Digital wallet within the Payment System to an 'E-commerce site' component, represented by a box containing a shopping cart icon and labeled 'E-commerce site'. The overall flow illustrates the process of depositing funds from a bank card into a digital wallet via the Payment System, ultimately enabling a purchase on an e-commerce website.
Figure 1 Digital wallet
Spending money is not the only feature that the digital wallet provides. For a payment platform like PayPal, we can directly transfer money to somebody else’s wallet on the same payment platform. Compared with the bank-to-bank transfer, direct transfer between digital wallets is faster, and most importantly, it usually does not charge an extra fee. Figure 2 shows a cross-wallet balance transfer operation.

Image represents a simplified model of a payment platform.  The diagram shows two square boxes, each labeled 'Digital wallet' and containing a dollar sign ($) symbol, representing two digital wallets.  A unidirectional arrow connects the two wallets, labeled 'Transfer,' indicating the flow of funds from one digital wallet to another.  The entire system is labeled 'Payment Platform' at the top, signifying that the transfer between the wallets is facilitated by this platform.  The arrangement visually depicts a transaction where money is moved from a source digital wallet to a destination digital wallet through the intermediary payment platform.
Figure 2 Cross-wallet balance transfer
Suppose we are asked to design the backend of a digital wallet application that supports the cross-wallet balance transfer operation. At the beginning of the interview, we will ask clarification questions to nail down the requirements.

Step 1 - Understand the Problem and Establish Design Scope
Candidate: Should we only focus on balance transfer operations between two digital wallets? Do we need to worry about other features?
Interviewer: Let’s focus on balance transfer operations only.

Candidate: How many transactions per second (TPS) does the system need to support?
Interviewer: Let’s assume 1,000,000 TPS.

Candidate: A digital wallet has strict requirements for correctness. Can we assume transactional guarantees [1] are sufficient?
Interviewer: That sounds good.

Candidate: Do we need to prove correctness?
Interviewer: This is a good question. Correctness is usually only verifiable after a transaction is complete. One way to verify is to compare our internal records with statements from banks. The limitation of reconciliation is that it only shows discrepancies and cannot tell how a difference was generated. Therefore, we would like to design a system with reproducibility, meaning we could always reconstruct historical balance by replaying the data from the very beginning.

Candidate: Can we assume the availability requirement is 99.99%
Interviewer: Sounds good.

Candidate: Do we need to take foreign exchange into consideration?
Interviewer: No, it’s out of scope.

In summary, our digital wallet needs to support the following:

Support balance transfer operation between two digital wallets.

Support 1,000,000 TPS.

Reliability is at least 99.99%.

Support transactions.

Support reproducibility.

Back-of-the-envelope estimation
When we talk about TPS, we imply a transactional database will be used. Today, a relational database running on a typical data center node can support a few thousand transactions per second. For example, reference [2] contains the performance benchmark of some of the popular transactional database servers. Let’s assume a database node can support 1,000 TPS. In order to reach 1 million TPS, we need 1,000 database nodes.

However, this calculation is slightly inaccurate. Each transfer command requires two operations: deducting money from one account and depositing money to the other account. To support 1 million transfers per second, the system actually needs to handle up to 2 million TPS, which means we need 2,000 nodes.

Table 1 shows the total number of nodes required when the “per-node TPS” (the TPS a single node can handle) changes. Assuming hardware remains the same, the more transactions a single node can handle per second, the lower the total number of nodes required, indicating lower hardware cost. So one of our design goals is to increase the number of transactions a single node can handle.

Per-node TPS	Node Number
100	20,000
1,000	2,000
10,000	200
Table 1 Mapping between pre-node TPS and node number


Step 2 - Propose High-Level Design and Get Buy-In
In this section, we will discuss the following:

API design

Three high-level designs

Simple in-memory solution

Database-based distributed transaction solution

Event sourcing solution with reproducibility

API Design
We will use the RESTful API convention. For this interview, we only need to support one API:

API	Detail
POST /v1/wallet/balance_transfer	Transfer balance from one wallet to another
Request parameters are:

Field	Description	Type
from_account	The debit account	string
to_account	The credit account	string
amount	The amount of money	string
currency	The currency type	string (ISO 4217 [3])
transaction_id	ID used for deduplication	uuid
Sample response body:

{
"Status": "success"
"Transaction_id": "01589980-2664-11ec-9621-0242ac130002"
}
One thing worth mentioning is that the data type of the “amount” field is “string,” rather than “double”. We explained the reasoning in the Payment System chapter.

In practice, many people still choose float or double representation of numbers because it is supported by almost every programming language and database. It is a proper choice as long as we understand the potential risk of losing precision.

In-memory sharding solution
The wallet application maintains an account balance for every user account. A good data structure to represent this <user,balance> relationship is a map, which is also called a hash table (map) or key-value store.

For in-memory stores, one popular choice is Redis. One Redis node is not enough to handle 1 million TPS. We need to set up a cluster of Redis nodes and evenly distribute user accounts among them. This process is called partitioning or sharding.

To distribute the key-value data among N partitions, we could calculate the hash value of the key and divide it by N. The remainder is the destination of the partition. The pseudocode below shows the sharding process:

String accountID = "A";
Int partitionNumber = 7;
Int myPartition = accountID.hashCode() % partitionNumber;
The number of partitions and addresses of all Redis nodes can be stored in a centralized place. We could use Zookeeper [4] as a highly-available configuration storage solution.

The final component of this solution is a service that handles the transfer commands. We call it the wallet service and it has several key responsibilities.

Receives the transfer command

Validates the transfer command

If the command is valid, it updates the account balances for the two users involved in the transfer. In a cluster, the account balances are likely to be in different Redis nodes

The wallet service is stateless. It is easy to scale horizontally. Figure 3 shows the in-memory solution.

Image represents a distributed system architecture for managing wallet balances, illustrating the flow of a transfer command. At the top, two circles labeled 'Transfer Command' represent inputs to the system. Below these, two rectangular boxes labeled 'Wallet Service' are positioned horizontally. The left 'Transfer Command' has a red arrow pointing down to the left 'Wallet Service', with the text 'A - $1 -> B' indicating a command to transfer $1 from account A to account B. From the left 'Wallet Service', two red arrows point downwards: one labeled 'A: - $1' goes to a rectangular box labeled '{ A: balance }' with 'Redis' below it, and another labeled 'B: + $1' goes to a box labeled '{ B: balance }' with 'Redis' below it. This visually depicts the balance updates performed by the Wallet Service for this specific transfer. The right 'Transfer Command' has a black arrow pointing down to the right 'Wallet Service'. From the right 'Wallet Service', black arrows point downwards to all three Redis boxes: '{ A: balance } Redis', '{ B: balance } Redis', and '{ C: balance } Redis'. Both 'Wallet Service' boxes have curved black arrows pointing to the right, converging on a cylinder shape labeled 'Partition Info' with 'Zookeeper' below it, indicating that the Wallet Services interact with Zookeeper to obtain partition information, presumably to determine which Redis instance holds the data for a given account. The diagram shows how transfer commands are processed by Wallet Services, which then update account balances stored in Redis, potentially using Zookeeper for coordination or lookup of data partitions.
Figure 3 In-memory solution
In this example, we have 3 Redis nodes. There are three clients, A, B, and C. Their account balances are evenly spread across these three Redis nodes. There are two wallet service nodes in this example that handle the balance transfer requests. When one of the wallet service nodes receives the transfer command which is to move $1 from client A to client B, it issues two commands to two Redis nodes. For the Redis node that contains client A’s account, the wallet service deducts $1 from the account. For client B, the wallet service adds $1 to the account.

Candidate: In this design, account balances are spread across multiple Redis nodes. Zookeeper is used to maintain the sharding information. The stateless wallet service uses the sharding information to locate the Redis nodes for the clients and updates the account balances accordingly.

Interviewer: This design works, but it does not meet our correctness requirement. The wallet service updates two Redis nodes for each transfer. There is no guarantee that both updates would succeed. If, for example, the wallet service node crashes after the first update has gone through but before the second update is done, it would result in an incomplete transfer. The two updates need to be in a single atomic transaction.

Distributed transactions
Database sharding
How do we make the updates to two different storage nodes atomic? The first step is to replace each Redis node with a transactional relational database node. Figure 4 shows the architecture. This time, clients A, B, and C are partitioned into 3 relational databases, rather than in 3 Redis nodes.

Image represents a system design for a wallet transfer functionality.  Two 'Transfer Command' inputs, represented as circles, initiate the transfer process. Each command is directed to a separate 'Wallet Service' rectangle, which acts as a processing unit.  These Wallet Services interact with three separate 'Relational Database' cylinders, each containing data with 'ID' and 'Balance' columns (example data shown: ID A, B, C with Balance 1 for each).  The Wallet Services access and update the balance information in these databases.  Crucially, both Wallet Services communicate with a 'Zookeeper' database cylinder labeled 'Partition Info,' indicating a distributed system architecture where Zookeeper manages the partitioning of data across the three relational databases.  The connections between the Wallet Services and the relational databases show that each Wallet Service can access and modify data across all three databases, suggesting a mechanism for distributing the load or handling data redundancy.
Figure 4 Relational database
Using transactional databases only solves part of the problem. As mentioned in the last section, it is very likely that one transfer command will need to update two accounts in two different databases. There is no guarantee that two update operations will be handled at exactly the same time. If the wallet service restarted right after it updated the first account balance, how can we make sure the second account will be updated as well?

Distributed transaction: two-phase commit
In a distributed system, a transaction may involve multiple processes on multiple nodes. To make a transaction atomic, the distributed transaction might be the answer. There are two ways to implement a distributed transaction: a low-level solution and a high-level solution. We will examine each of them.

The low-level solution relies on the database itself. The most commonly used algorithm is called two-phase commit (2PC). As the name implies, it has two phases, as in Figure 5.

Image represents a two-phase commit protocol for a distributed transaction involving a coordinator (labeled 'Coordinator (Wallet Service)') and two databases (labeled 'Database A' and 'Database C').  The horizontal axis represents time, and the diagram shows the sequence of events. The coordinator initiates the transaction by sending 'write data' requests to both databases.  Each database then acquires a lock (indicated by 'lock A' and 'lock C') on its respective data before acknowledging with 'ok' to the coordinator. This constitutes phase 1 ('Prepare').  Once the coordinator receives 'ok' from both databases, it enters phase 2 ('Commit'), sending a commit signal.  Upon receiving the commit signal, each database writes the data, indicated by the red lines representing the actual data write, and releases its lock ('unlock A' and 'unlock C'), sending 'ok' back to the coordinator to confirm successful completion.  The entire process is visualized using circles representing the different stages of the transaction and arrows indicating the flow of messages and data between the coordinator and the databases.
Figure 5 Two-phase commit (source [5])
The coordinator, which in our case is the wallet service, performs read and write operations on multiple databases as normal. As shown in Figure 5, both databases A and C are locked.

When the application is about to commit the transaction, the coordinator asks all databases to prepare the transaction.

In the second phase, the coordinator collects replies from all databases and performs the following:

If all databases reply with a “yes”, the coordinator asks all databases to commit the transaction they have received.

If any database replies with a “no”, the coordinator asks all databases to abort the transaction.

It is a low-level solution because the prepare step requires a special modification to the database transaction. For example, there is an X/Open XA [6] standard that coordinates heterogeneous databases to achieve 2PC. The biggest problem with 2PC is that it’s not performant, as locks can be held for a very long time while waiting for a message from the other nodes. Another issue with 2PC is that the coordinator can be a single point of failure, as shown in Figure 6.

Image represents a simplified system architecture illustrating a distributed transaction involving a coordinator (labeled 'Coordinator (Wallet Service)') and two databases (labeled 'Database A' and 'Database C').  The coordinator initiates a write operation to both databases sequentially.  Data flows are represented by arrows. The coordinator first sends 'write data' requests to Database A, receiving an 'ok' acknowledgment. This process repeats for Database C.  A dashed line indicates a point in time. After the initial writes, the coordinator enters a 'Prepare' phase, indicated by a label.  However, before the prepare phase completes, the coordinator crashes, marked with a red 'X Crashed'.  Following the crash, the databases independently respond with 'yes' to the implicit prepare phase request, indicating their readiness to commit the transaction.  The time axis shows the progression of events, with the 'X' marks indicating the coordinator's failure and the subsequent independent responses from the databases.  The diagram visually depicts a potential failure scenario in a distributed transaction and the subsequent handling (or lack thereof) by the databases.
Figure 6 Coordinator crashes
Distributed transaction: Try-Confirm/Cancel (TC/C)
TC/C is a type of compensating transaction [7] that has two steps:

In the first phase, the coordinator asks all databases to reserve resources for the transaction.

In the second phase, the coordinator collects replies from all databases:

If all databases reply with “yes”, the coordinator asks all databases to confirm the operation, which is the Try-Confirm process.

If any database replies with “no”, the coordinator asks all databases to cancel the operation, which is the Try-Cancel process.

It’s important to note that the two phases in 2PC are wrapped in the same transaction, but in TC/C each phase is a separate transaction.

TC/C example

It would be much easier to explain how TC/C works with a real-world example. Suppose we want to transfer $1 from account A to account C. Table 2 gives a summary of how TC/C is executed in each phase.

Phase	Operation	A	C
1	Try	Balance change: -$1	Do nothing
2	Confirm	Do nothing	Balance change: +$1
Cancel	Balance change: +$1	Do Nothing
Table 2 TC/C example

Let’s assume the wallet service is the coordinator of the TC/C. At the beginning of the distributed transaction, account A has $1 in its balance, and account C has $0.

First phase: Try

In the Try phase, the wallet service, which acts as the coordinator, sends two transaction commands to two databases:

For the database that contains account A, the coordinator starts a local transaction that reduces the balance of A by $1.

For the database that contains account C, the coordinator gives it a NOP (no operation.) To make the example adaptable for other scenarios, let’s assume the coordinator sends to this database a NOP command. The database does nothing for NOP commands and always replies to the coordinator with a success message.

The Try phase is shown in Figure 7. The thick line indicates that a lock is held by the transaction.

Image represents a sequence diagram depicting the 'First Phase' of a distributed transaction, likely the 'Try' phase of a two-phase commit or similar protocol. The diagram shows three horizontal timelines: 'Coordinator (Wallet Service)' at the top, 'Database A' in the middle, and 'Database C' at the bottom. A dashed vertical line indicates the starting point of the interactions. The process begins with a circle on the Coordinator timeline. From this point, two simultaneous interactions originate: an arrow labeled 'Try: A-$1' points diagonally downwards to a circle labeled 'A=$1' on the Database A timeline, and another arrow labeled 'NOP' (No Operation) points diagonally downwards to a circle labeled 'C=$0' on the Database C timeline. On the Database A timeline, following the 'A=$1' circle, a thick red horizontal line segment indicates an action occurring over time. Below this segment, the text 'lock A' is placed near the start and 'unlock A' near the end, suggesting that account A is locked during this operation. Above the red segment, the text 'UPDATE balance SET amount=amount-1 WHERE account='A'' is displayed, indicating the database operation being performed. After the red segment on the Database A timeline, there is another circle labeled 'A=$0', showing the updated balance of account A. An arrow labeled with a blue checkmark and 'Done' points diagonally upwards from this circle on Database A back to the Coordinator timeline, indicating completion of the operation at Database A. On the Database C timeline, following the 'NOP' message, a horizontal line segment labeled 'NOP' spans some time, followed by a circle labeled 'C=$0'. An arrow labeled with a blue checkmark and 'Done' points diagonally upwards from this circle on Database C back to the Coordinator timeline, indicating completion at Database C. The diagram illustrates the Coordinator sending 'try' requests to the databases, with Database A performing the balance update for account A under a lock, and both databases signaling completion back to the Coordinator in this first phase.
Figure 7 Try phase
Second phase: Confirm

If both databases reply “yes”, the wallet service starts the next Confirm phase.

Account A’s balance has already been updated in the first phase. The wallet service does not need to change its balance. However, account C has not yet received its $1 from account A in the first phase. In the Confirm phase, the wallet service has to add $1 to account C’s balance.

The Confirm process is shown in Figure 8.

Image represents a sequence diagram illustrating a two-phase distributed transaction, divided into a 'First Phase' and a 'Second Phase: Confirm' by a vertical dashed line. The diagram shows the interactions between three participants, each represented by a horizontal timeline: 'Coordinator (Wallet Service)' at the top, 'Database A' in the middle, and 'Database C' at the bottom. In the 'First Phase', starting from an event on the Coordinator timeline, the Coordinator sends a 'Try: A-$1' message diagonally downwards to Database A, which reaches a state indicated by a circle labeled 'A=$1'. Database A then performs an action represented by a thick red horizontal line segment, labeled 'lock A' at its start and 'unlock A' at its end, with the text 'UPDATE balance SET amount=amount-1 WHERE account='A'' above it, signifying the decrement of account A's balance under a lock. After this action, Database A reaches a state labeled 'A=$0'. Concurrently, the Coordinator sends a 'NOP' (No Operation) message diagonally downwards to Database C, which reaches a state labeled 'C=$0' and performs a 'NOP' action represented by a black horizontal line. Both Database A and Database C send a 'Done' message with a blue checkmark diagonally upwards back to the Coordinator, indicating the completion of the first phase on their respective ends. The diagram then transitions to the 'Second Phase: Confirm'. Starting from a subsequent event on the Coordinator timeline, the Coordinator sends a 'NOP' message to Database A, which remains in state 'A=$0' and performs another 'NOP' action before reaching the same state again and sending a 'Done' message back to the Coordinator. Simultaneously in the second phase, the Coordinator sends a 'Confirm: C+$1' message diagonally downwards to Database C, which is in state 'C=$0'. Database C then performs an action represented by a thick red horizontal line segment, labeled 'lock C' at its start and 'unlock A' at its end (likely a typo for 'unlock C'), with the text 'UPDATE balance SET amount=amount+1 WHERE account='C'' below it, signifying the increment of account C's balance under a lock. After this action, Database C reaches a state labeled 'C=$1' and sends a 'Done' message back to the Coordinator. The diagram visually represents the two-phase commit flow where the first phase involves a 'try' request and tentative changes (like decrementing A), and the second phase involves confirming or completing the transaction (like incrementing C), with NOPs sent to databases that don't require further changes in a given phase, and locks/unlocks potentially used for concurrency control.
Figure 8 Confirm phase
Second phase: Cancel

What if the first Try phase fails? In the example above we have assumed the NOP operation on account C always succeeds, although in practice it may fail. For example, account C might be an illegal account, and the regulator has mandated that no money can flow into or out of this account. In this case, the distributed transaction must be canceled and we have to clean up.

Because the balance of account A has already been updated in the transaction in the Try phase, it is impossible for the wallet service to cancel a completed transaction. What it can do is to start another transaction that reverts the effect of the transaction in the Try phase, which is to add $1 back to account A.

Because account C was not updated in the Try phase, the wallet service just needs to send a NOP operation to account C’s database.

The Cancel process is shown in Figure 9.

Image represents a sequence diagram illustrating a two-phase distributed transaction process with a 'Cancel' outcome in the second phase, involving a 'Coordinator (Wallet Service)' and two databases, 'Database A' and 'Database C', each represented by a horizontal timeline. The diagram is divided by dashed vertical lines into a 'First Phase' and a 'Second Phase: Cancel'. In the 'First Phase', the Coordinator sends a 'Try: A-$1' message to Database A, which updates account A's balance by decrementing $1 under a lock (shown by a red bar with 'lock A' and 'unlock A' labels and 'UPDATE balance SET amount=amount-1 WHERE account='A'' text), changing the state from 'A=$1' to 'A=$0', and then sends a 'Done' message back to the Coordinator. Simultaneously, the Coordinator sends a 'NOP' message to Database C, which performs a No Operation (shown by a black bar), maintaining the state 'C=$0', and also sends a 'Done' message back. However, below the 'Done' message from Database C, a red 'X Failed' is shown, indicating that despite sending 'Done', the first phase transaction did not complete successfully overall, likely due to a failure condition related to Database C, triggering the 'Second Phase: Cancel'. In the 'Second Phase: Cancel', the Coordinator initiates cancellation by sending a 'Cancel: A+$1' message to Database A, which is in state 'A=$0'. Database A performs a compensating action (shown by a red bar, labeled with 'lock C' and 'unlock A' which are inconsistent with the message and surrounding state changes but the text 'UPDATE balance SET amount=amount+1 WHERE account='A'' above the timeline indicates the rollback), changing the state back to 'A=$1', and sends a 'Done' message. Concurrently, the Coordinator sends a 'NOP' message to Database C, which performs a NOP (black bar), maintains the state 'C=$0', and sends a 'Done' message. The diagram visually demonstrates the process of attempting a distributed transaction in the first phase, encountering a failure (implied by the red 'X Failed' related to Database C), and then executing a cancellation or rollback in the second phase, specifically undoing the changes made to account A in the first phase to restore consistency.
Figure 9 Cancel phase
Comparison between 2PC and TC/C

Table 3 shows that there are many similarities between 2PC and TC/C, but there are also differences. In 2PC, all local transactions are not done (still locked) when the second phase starts, while in TC/C, all local transactions are done (unlocked) when the second phase starts. In other words, the second phase of 2PC is about completing an unfinished transaction, such as an abort or commit, while in TC/C, the second phase is about using a reverse operation to offset the previous transaction result when an error occurs. The following table summarizes their differences.

First Phase	Second Phase: success	Second Phase: fail
2PC	Local transactions are not done yet	Commit all local transactions Cancel all local transactions	Cancel all local transactions
TC/C	
All local transactions are completed, either committed or canceled

Execute new local transactions if needed	
Reverse the side effect of the already committed transaction, or called “undo”

Table 3 2PC vs TC/C

TC/C is also called a distributed transaction by compensation. It is a high-level solution because the compensation, also called the “undo,” is implemented in the business logic. The advantage of this approach is that it is database-agnostic. As long as a database supports transactions, TC/C will work. The disadvantage is that we have to manage the details and handle the complexity of the distributed transactions in the business logic at the application layer.

Phase status table

We still have not yet answered the question asked earlier; what if the wallet service restarts in the middle of TC/C? When it restarts, all previous operation history might be lost, and the system may not know how to recover.

The solution is simple. We can store the progress of a TC/C as phase status in a transactional database. The phase status includes at least the following information.

The ID and content of a distributed transaction.

The status of the Try phase for each database. The status could be “not sent yet”, “has been sent”, and “response received”.

The name of the second phase. It could be “Confirm” or “Cancel.” It could be calculated using the result of the Try phase.

The status of the second phase.

An out-of-order flag (explained soon in the section “Out-of-Order Execution”).

Where should we put the phase status tables? Usually, we store the phase status in the database that contains the wallet account from which money is deducted. The updated architecture diagram is shown in Figure 10.

Image represents a system architecture for handling transfer commands.  Two identical 'Transfer Command' inputs, represented as circles, initiate the process. Each input points to a 'Wallet Service' rectangle.  Each 'Wallet Service' interacts with three cylindrical database instances, each containing a 'Phase Status Table' (pink rectangle) and a 'Balance Table' (white rectangle).  The 'Wallet Services' are interconnected, with lines indicating data flow between them.  Crucially, both 'Wallet Services' also connect to a 'Zookeeper' database cylinder labeled 'Partition Info,' suggesting coordination and data consistency across the system.  The connections imply that the 'Wallet Services' use the 'Partition Info' in Zookeeper to determine which database instance to update for a given transaction, ensuring data is distributed across the three database instances.
Figure 10 Phase status table
Unbalanced state

Have you noticed that by the end of the Try phase, $1 is missing (Figure 11)?

Assuming everything goes well, by the end of the Try phase, $1 is deducted from account A and account C remains unchanged. The sum of account balances in A and C will be $0, which is less than at the beginning of the TC/C. It violates a fundamental rule of accounting that the sum should remain the same after a transaction.

The good news is that the transactional guarantee is still maintained by TC/C. TC/C comprises several independent local transactions. Because TC/C is driven by application, the application itself is able to see the intermediate result between these local transactions. On the other hand, the database transaction or 2PC version of the distributed transaction was maintained by databases that are invisible to high-level applications.

There are always data discrepancies during the execution of distributed transactions. The discrepancies might be transparent to us because lower-level systems such as databases already fixed the discrepancies. If not, we have to handle it ourselves (for example, TC/C).

The unbalanced state is shown in Figure 11.

Image represents a sequence diagram illustrating the 'First Phase' and 'Second Phase: Confirm' of a distributed transaction (likely a Try-Confirm-Cancel or two-phase commit pattern) involving a 'Coordinator (Wallet Service)', 'Database A', and 'Database C'. The diagram shows three horizontal timelines for these participants, separated by dashed vertical lines marking the start and end of the phases. In the 'First Phase', starting from an event on the Coordinator timeline, a 'Try: A-$1' message is sent to Database A, which is in state 'A=$1'. Database A then performs an operation (represented by a red bar) labeled 'lock A' at the start and 'unlock A' at the end, with the text 'UPDATE balance SET amount=amount-1 WHERE account='A'' above the timeline, resulting in state 'A=$0', and sends a 'Done' message back to the Coordinator. Concurrently, the Coordinator sends a 'NOP' message to Database C, in state 'C=$0', which performs a No Operation (black bar), remains in state 'C=$0', and sends a 'Done' message back. Below the diagram, a label 'Before TCC start: A+C = $1' is shown before the first phase, connected by an arrow to the start dashed line. An arrow curves from the 'Done' events in the first phase down to a box labeled 'After first phase: A+C = $0', highlighted in red, with the annotation 'Unbalanced state' below and a label 'Money loss: $1' curving towards it. In the 'Second Phase: Confirm', starting from another event on the Coordinator timeline, a 'NOP' message is sent to Database A, which remains in state 'A=$0', performs a NOP, and sends a 'Done' message. Simultaneously, a 'Confirm: C+$1' message is sent to Database C, in state 'C=$0', which performs an operation (red bar) labeled 'lock C' at the start and 'unlock A' at the end (likely a typo for 'unlock C'), with 'UPDATE balance SET amount=amount+1 WHERE account='C'' below the timeline, resulting in state 'C=$1', and sends a 'Done' message. An arrow curves from the 'Done' events in the second phase down to a label 'After second phase: A+C = $1', highlighted in blue, with a label 'Money recovery: $1' curving towards it. The diagram illustrates how the first phase performs a tentative debit, potentially leaving the system in an unbalanced state where the total sum of A and C is reduced, and the second phase confirms the transaction by performing the corresponding credit, restoring the balance of the total sum, representing a successful transfer from A to C.
Figure 11 Unbalanced state
Valid operation orders

There are three choices for the Try phase:

Try phase choices	Account A	Account C
Choice 1	-$1	NOP
Choice 2	NOP	+$1
Choice 3	-$1	+$1
Table 4 Try phase choices

All three choices look plausible, but some are not valid.

For choice 2, if the Try phase on account C is successful, but has failed on account A (NOP), the wallet service needs to enter the Cancel phase. There is a chance that somebody else may jump in and move the $1 away from account C. Later when the wallet service tries to deduct $1 from account C, it finds nothing is left, which violates the transactional guarantee of a distributed transaction.

For choice 3, if $1 is deducted from account A and added to account C concurrently, it introduces lots of complications. For example, $1 is added to account C, but it fails to deduct the money from account A. What should we do in this case?

Therefore, choice 2 and choice 3 are flawed choices and only choice 1 is valid.

Out-of-order execution

One side effect of TC/C is the out-of-order execution. It will be much easier to explain using an example.

We reuse the above example which transfers $1 from account A to account C. As Figure 12 shows, in the Try phase, the operation against account A fails and it returns a failure to the wallet service, which then enters the Cancel phase and sends the cancel operation to both account A and account C.

Let’s assume that the database that handles account C has some network issues and it receives the Cancel instruction before the Try instruction. In this case, there is nothing to cancel.

The out-of-order execution is shown in Figure 12.

Image represents a system design diagram illustrating a two-phase operation involving a coordinator (labeled 'Coordinator (Wallet Service)') and two databases (labeled 'Database A' and 'Database C').  The first phase shows the coordinator initiating a 'Try' operation, sending requests to Database A. Database A processes the request and forwards it to another component within Database A. This first phase ends with a failure indicated by 'X Failed'. The second phase, labeled 'Second Phase: Cancel,' depicts the coordinator attempting to cancel the previous operation.  It sends 'Cancel' requests to both Database A and Database C.  However, Database C receives the 'Try' operation from the coordinator *after* receiving the 'Cancel' operation, highlighting an out-of-order arrival indicated by 'Out-of-order: Cancel operation arrived before Try operation'.  Database C's response to the 'Try' operation is labeled 'Try', while Database A receives and processes both 'Cancel' requests.  The diagram uses circles to represent processing points and arrows to show the flow of requests and responses between components.  The dashed vertical lines separate the two phases.
Figure 12 Out-of-order execution
To handle out-of-order operations, each node is allowed to Cancel a TC/C without receiving a Try instruction, by enhancing the existing logic with the following updates:

The out-of-order Cancel operation leaves a flag in the database indicating that it has seen a Cancel operation, but it has not seen a Try operation yet.

The Try operation is enhanced so it always checks whether there is an out-of-order flag, and it returns a failure if there is.

This is why we added an out-of-order flag to the phase status table in the “Phase Status Table” section.

Distributed transaction: Saga
Linear order execution

There is another popular distributed transaction solution called Saga [8]. Saga is the de-facto standard in a microservice architecture. The idea of Saga is simple:

All operations are ordered in a sequence. Each operation is an independent transaction on its own database.

Operations are executed from the first to the last. When one operation has finished, the next operation is triggered.

When an operation has failed, the entire process starts to roll back from the current operation to the first operation in reverse order, using compensating transactions. So if a distributed transaction has n operations, we need to prepare 2n operations: n for the normal case and another n for the compensating transaction during rollback.

It is easier to understand this by using an example. Figure 13 shows the Saga workflow to transfer $1 from account A to account C. The top horizontal line shows the normal order of execution. The two vertical lines show what the system should do when there is an error. When it encounters an error, the transfer operations are rolled back and the client receives an error message. As we mentioned in the “Valid operation orders” section, we have to put the deduction operation before the addition operation.

Image represents a flow diagram illustrating a transaction process with error handling branching. The flow starts from a circle labeled 'Start'. An arrow leads from 'Start' to a rectangular box labeled 'A-$1', representing an action to subtract $1 from account A. From 'A-$1', a horizontal arrow points to a rectangular box labeled 'C+$1', representing an action to add $1 to account C. An arrow from 'C+$1' leads to a circle containing a blue checkmark, labeled 'Success', indicating the successful completion of the transaction. Below the 'A-$1' box, a branch labeled 'Error' leads downwards via an arrow to a rectangular box labeled 'A+$1', representing a compensating action to add $1 back to account A in case of an error after the A-$1 step. An arrow from 'A+$1' points downwards to a circular symbol containing a red cross within another circle, labeled 'Error', indicating an unsuccessful state after the compensating action. Similarly, below the 'C+$1' box, a branch labeled 'Error' leads downwards via an arrow to a rectangular box labeled 'C-$1', representing a compensating action to subtract $1 from account C in case of an error after the C+$1 step. An arrow from 'C-$1' leads downwards to a rectangular box labeled 'A+$1', representing another compensating action (adding $1 back to A). An arrow from this second 'A+$1' box points downwards to another circular symbol with a red cross, labeled 'Error', indicating an unsuccessful state in this error handling path. The diagram depicts a sequence of operations (debit A, credit C) and the corresponding rollback or compensation logic when errors occur at different stages.
Figure 13 Saga workflow
How do we coordinate the operations? There are two ways to do it:

Choreography. In a microservice architecture, all the services involved in the Saga distributed transaction do their jobs by subscribing to other services’ events. So it is fully decentralized coordination.

Orchestration. A single coordinator instructs all services to do their jobs in the correct order.

The choice of which coordination model to use is determined by the business needs and goals. The challenge of the choreography solution is that services communicate in a fully asynchronous way, so each service has to maintain an internal state machine in order to understand what to do when other services emit an event. It can become hard to manage when there are many services. The orchestration solution handles complexity well, so it is usually the preferred solution in a digital wallet system.

Comparison between TC/C and Saga

TC/C and Saga are both application-level distributed transactions. Table 5 summarizes their similarities and differences.

TC/C	Saga
Compensating action	In Cancel phase	In rollback phase
Central coordination	Yes	Yes (orchestration mode)
Operation execution order	any	linear
Parallel execution possibility	Yes	No (linear execution)
Could see the partial inconsistent status	Yes	Yes
Application or database logic	Application	Application
Table 5 TC/C vs Saga

Which one should we use in practice? The answer depends on the latency requirement. As Table 5 shows, operations in Saga have to be executed in linear order, but it is possible to execute them in parallel in TC/C. So the decision depends on a few factors:

If there is no latency requirement, or there are very few services, such as our money transfer example, we can choose either of them. If we want to go with the trend in microservice architecture, choose Saga.

If the system is latency-sensitive and contains many services/operations, TC/C might be a better option.

Candidate: To make the balance transfer transactional, we replace Redis with a relational database, and use TC/C or Saga to implement distributed transactions.

Interviewer: Great work! The distributed transaction solution works, but there might be cases where it doesn’t work well. For example, users might enter the wrong operations at the application level. In this case, the money we specified might be incorrect. We need a way to trace back the root cause of the issue and audit all account operations. How can we do this?

Event sourcing
Background
In real life, a digital wallet provider may be audited. These external auditors might ask some challenging questions, for example:

Do we know the account balance at any given time?

How do we know the historical and current account balances are correct?

How do we prove that the system logic is correct after a code change?

One design philosophy that systematically answers those questions is event sourcing, which is a technique developed in Domain-Driven Design (DDD) [9].

Definition
There are four important terms in event sourcing.

Command

Event

State

State machine

Command

A command is the intended action from the outside world. For example, if we want to transfer $1 from client A to client C, this money transfer request is a command.

In event sourcing, it is very important that everything has an order. So commands are usually put into a FIFO (first in, first out) queue.

Event

Command is an intention and not a fact because some commands may be invalid and cannot be fulfilled. For example, the transfer operation will fail if the account balance becomes negative after the transfer.

A command must be validated before we do anything about it. Once the command passes the validation, it is valid and must be fulfilled. The result of the fulfillment is called an event.

There are two major differences between command and event.

Events must be executed because they represent a validated fact. In practice, we usually use the past tense for an event. If the command is “transfer $1 from A to C”, the corresponding event would be “transferred $1 from A to C”.

Commands may contain randomness or I/O, but events must be deterministic. Events represent historical facts.

There are two important properties of the event generation process.

One command may generate any number of events. It could generate zero or more events.

Event generation may contain randomness, meaning it is not guaranteed that a command always generates the same event(s). The event generation may contain external I/O or random numbers. We will revisit this property in more detail near the end of the chapter.

The order of events must follow the order of commands. So events are stored in a FIFO queue, as well.

State

State is what will be changed when an event is applied. In the wallet system, state is the balances of all client accounts, which can be represented with a map data structure. The key is the account name or ID, and the value is the account balance. Key-value stores are usually used to store the map data structure. The relational database can also be viewed as a key-value store, where keys are primary keys and values are table rows.

State machine

A state machine drives the event sourcing process. It has two major functions.

Validate commands and generate events.

Apply event to update state.

Event sourcing requires the behavior of the state machine to be deterministic. Therefore, the state machine itself should never contain any randomness. For example, it should never read anything random from the outside using I/O, or use any random numbers. When it applies an event to a state, it should always generate the same result.

Figure 14 shows the static view of event sourcing architecture. The state machine is responsible for converting the command to an event and for applying the event. Because state machine has two primary functions, we usually draw two state machines, one for validating commands and the other for applying events.

Image represents a system architecture diagram illustrating a command-based state machine.  The process begins with a `Command` (represented by a rectangle) which is input into the first `State Machine` (represented by a gear-like shape labeled 'State Machine'). This state machine validates the command, indicated by an arrow labeled 'Validate,' and passes the validated command to an `Event` (represented by a rectangle). The `Event` is then fed into a second `State Machine` (also a gear-like shape labeled 'State Machine'), where it's applied.  Both state machines interact with a central `State` database (represented by a cylinder labeled 'State'). The first state machine reads the current state from the database, indicated by an arrow labeled 'Read,' before validation.  The second state machine applies the event to update the state in the database, indicated by an arrow labeled 'Apply.'  The overall flow depicts a system where commands are processed, validated, transformed into events, and applied to update the system's state, stored persistently in the database.
Figure 14 Static view of event sourcing
If we add the time dimension, Figure 15 shows the dynamic view of event sourcing. The system keeps receiving commands and processing them, one by one.

Image represents a state machine diagram illustrating a system's behavior over time.  Three horizontal lines represent the flow of information: the top line shows the progression of commands, the middle line tracks events, and the bottom line depicts the system's state.  Light-blue circles represent command inputs, while pale-yellow circles represent events generated within the system.  Pink circles represent the system's state at different points in time.  Arrows indicate the flow of information.  A command input (light-blue circle) triggers a validation process (indicated by an arrow labeled 'Validate') resulting in an event (pale-yellow circle). This event is then 'Applied' (indicated by an arrow with this label) to update the system's state (pink circle). This process repeats across three cycles, showing the system transitioning through different states in response to commands and generating corresponding events.  Finally, each command, event, and resulting state are shown flowing to their respective output lines labeled 'Command,' 'Event,' and 'State' (with 'Time' indicated below the state line to emphasize the temporal aspect).
Figure 15 Dynamic view of event sourcing
Wallet service example
For the wallet service, the commands are balance transfer requests. These commands are put into a FIFO queue. One popular choice for the command queue is Kafka [10]. The command queue is shown in Figure 16.

Image represents a system flow where commands are processed through a queue implemented with Kafka. The process begins with a rectangular box containing the text 'A -$1-> C', which signifies a command, possibly a transfer of $1 from account A to account C. An arrow labeled 'in' points from this command box into a larger rectangular box labeled 'Command Queue' below which 'Kafka' is written. Inside the 'Command Queue' box, there are three smaller rectangular boxes arranged horizontally, representing commands currently in the queue. From left to right, these commands are labeled 'A -$1-> C', 'A -$1-> B', and 'A -$1-> D', indicating different transfer commands queued up. An arrow labeled 'out' originates from the right side of the 'Command Queue' box and points to a circle, which typically signifies the end of a process or output point. The diagram illustrates commands entering a queue (Kafka), being stored within it, and then exiting the queue for subsequent processing (represented by the 'out' arrow and final circle).
Figure 16 Command queue
Let us assume the state (the account balance) is stored in a relational database. The state machine examines each command one by one in FIFO order. For each command, it checks whether the account has a sufficient balance. If yes, the state machine generates an event for each account. For example, if the command is “A->$1->C”, the state machine generates two events: “A:-$1” and “C:+$1”.

Figure 17 shows how the state machine works in 5 steps.

Read commands from the command queue.

Read balance state from the database.

Validate the command. If it is valid, generate two events for each of the accounts.

Read the next Event.

Apply the Event by updating the balance in the database.

Image represents a system architecture utilizing command and event queues for processing transactions and maintaining state. A 'Client' initiates a transaction with a command, shown as a rectangular box labeled 'A -$1-> C', which enters the system via an arrow labeled 'in' into a larger rectangular box representing a 'Command Queue', specified as 'Kafka'. Inside the 'Command Queue' are three smaller boxes representing queued commands: 'A -$1-> C' (yellow), 'A -$1-> B' (cyan), and 'A -$1-> D' (green). Below the 'Command Queue' is another rectangular box labeled 'Event Queue'. Dashed arrows point from each command box in the 'Command Queue' downwards to two corresponding boxes in the 'Event Queue' with matching colors: 'A -$1-> C' corresponds to yellow boxes 'A: -$1' and 'C: +$1'; 'A -$1-> B' corresponds to cyan boxes 'A: -$1' and 'B: +$1'; and 'A -$1-> D' corresponds to green boxes 'A: -$1' and 'D: +$1'. To the right of the queues, a spiked circle labeled 'State Machine' is shown. Dashed lines from the 'Command Queue' point towards this State Machine, with one labeled 'Validate Commands'. An arrow labeled '2 Read' points from this State Machine to a cylinder shape labeled 'State', which contains account balances 'A: $5', 'B: $4', 'C: $3', 'D: $2'. Arrows labeled '3' point from this State Machine downwards to the 'Event Queue', indicating that the State Machine generates events based on validating commands and reading the current state. Below the 'Event Queue', another spiked circle, also labeled 'State Machine', is shown. An arrow labeled '4 Read' points from this lower State Machine to the 'Event Queue'. An arrow labeled '5 Update' points from the lower State Machine back up to the 'State' cylinder. A dashed line from the lower State Machine is labeled 'Apply Events'. The diagram illustrates a Command-Event Sourcing pattern where commands are queued, a State Machine processes commands by reading the state and emitting events, and another State Machine consumes events from an event queue to update the system state.
Figure 17 How state machine works
Reproducibility
The most important advantage that event sourcing has over other architectures is reproducibility.

In the distributed transaction solutions mentioned earlier, a wallet service saves the updated account balance (the state) into the database. It is difficult to know why the account balance was changed. Meanwhile, historical balance information is lost during the update operation. In the event sourcing design, all changes are saved first as immutable history. The database is only used as an updated view of what balance looks like at any given point in time.

We could always reconstruct historical balance states by replaying the events from the very beginning. Because the event list is immutable and the state machine logic is deterministic, it is guaranteed that the historical states generated from each replay are the same.

Figure 18 shows how to reproduce the states of the wallet service by replaying the events.

Image represents a system flow involving a 'Command Queue', an 'Event Queue', and 'Historical States'. At the top is a rectangular box labeled 'Command Queue', containing three smaller rectangular boxes representing commands: 'A -$1-> C' (yellow), 'A -$1-> B' (cyan), and 'A -$1-> D' (green), arranged horizontally from left to right. Below the 'Command Queue' is another rectangular box labeled 'Event Queue', containing six smaller rectangular boxes representing events, also arranged horizontally. These events correspond to the commands above, with dashed arrows pointing from each command box to the corresponding event boxes in the 'Event Queue'. Specifically, from 'A -$1-> C' (yellow) in the 'Command Queue', dashed arrows point to two yellow boxes in the 'Event Queue' labeled 'A: -$1' and 'C: +$1'. From 'A -$1-> B' (cyan), dashed arrows point to two cyan boxes labeled 'A: -$1' and 'B: +$1'. From 'A -$1-> D' (green), dashed arrows point to two green boxes labeled 'A: -$1' and 'D: +$1'. Below the 'Event Queue', three cylinder shapes are arranged horizontally, each labeled 'Historical State'. The first 'Historical State' cylinder lists balances 'A: $5', 'B: $4', 'C: $3', 'D: $2'. Dashed arrows point from the yellow event boxes ('A: -$1', 'C: +$1') down to this first cylinder. A curved arrow points from this first cylinder to the second 'Historical State' cylinder. The second cylinder lists balances 'A: $4' (in red), 'B: $4', 'C: $4' (in red), 'D: $2'. Dashed arrows point from the cyan event boxes ('A: -$1', 'B: +$1') down to this second cylinder. A curved arrow points from the second cylinder to the third 'Historical State' cylinder. The third cylinder lists balances 'A: $3' (in red), 'B: $5' (in red), 'C: $4', 'D: $2'. Dashed arrows point from the green event boxes ('A: -$1', 'D: +$1') down to this third cylinder. The diagram illustrates that commands are processed from the 'Command Queue', generating events that are placed in the 'Event Queue'. These events are then applied to the system state, creating a sequence of 'Historical States' as events are processed over time, demonstrating an event-driven architecture and the evolution of state.
Figure 18 Reproduce states
Reproducibility helps us answer the difficult questions that the auditors ask at the beginning of the section. We repeat the questions here.

Do we know the account balance at any given time?

How do we know the historical and current account balances are correct?

How do we prove the system logic is correct after a code change?

For the first question, we could answer it by replaying events from the start, up to the point in time where we would like to know the account balance.

For the second question, we could verify the correctness of the account balance by recalculating it from the event list.

For the third question, we can run different versions of the code against the events and verify that their results are identical.

Because of the audit capability, event sourcing is often chosen as the de facto solution for the wallet service.

Command-query responsibility segregation (CQRS)
So far, we have designed the wallet service to move money from one account to another efficiently. However, the client still does not know what the account balance is. There needs to be a way to publish state (balance information) so the client, which is outside of the event sourcing framework, can know what the state is.

Intuitively, we can create a read-only copy of the database (historical state) and share it with the outside world. Event sourcing answers this question in a slightly different way.

Rather than publishing the state (balance information), event sourcing publishes all the events. The external world could rebuild any customized state itself. This design philosophy is called CQRS [11].

In CQRS, there is one state machine responsible for the write part of the state, but there can be many read-only state machines, which are responsible for building views of the states. Those views could be used for queries.

These read-only state machines can derive different state representations from the event queue. For example, clients may want to know their balances and a read-only state machine could save state in a database to serve the balance query. Another state machine could build state for a specific time period to help investigate issues like possible double charges. The state information is an audit trail that could help to reconcile the financial records.

The read-only state machines lag behind to some extent, but will always catch up. The architecture design is eventually consistent.

Figure 19 shows a classic CQRS architecture.

Image represents a system architecture divided into a 'Write Path' and a 'Read Path', demonstrating a Command Query Responsibility Segregation (CQRS) pattern with Event Sourcing. The 'Write Path' at the top handles transactional operations. It starts with a 'Client' sending a command, depicted as a rectangular box labeled 'A -$1-> C', which enters the 'Write Path' via an 'in' arrow into a 'Command Queue'. The 'Command Queue', identified as 'Kafka', contains multiple queued commands shown as colored boxes: 'A -$1-> C' (yellow), 'A -$1-> B' (cyan), and 'A -$1-> D' (green). A spiked circle labeled 'State Machine' processes these commands; dashed arrows indicate it 'Validate Commands'. The State Machine interacts with a cylinder labeled 'State' (listing balances A:$5, B:$4, C:$3, D:$2) via a '2 Read' arrow and a subsequent '5 Update' arrow originating from a separate State Machine below. The State Machine within the Write Path generates events based on commands and state, indicated by '3' arrows pointing down to the 'Event Queue'. The 'Event Queue' below the 'Command Queue' stores these generated events in corresponding colored boxes: yellow command generates 'A: -$1' and 'C: +$1' events, cyan command generates 'A: -$1' and 'B: +$1' events, and green command generates 'A: -$1' and 'D: +$1' events. Events from the 'Event Queue' are published via a dashed arrow labeled 'Publish Events', initiating processing in the 'Read Path'. The 'Read Path' is a rectangular box below and to the right. Inside the 'Read Path', events are read by a spiked circle labeled 'State Machine' via a '4 Read' arrow from the Event Queue. This State Machine is responsible for rebuilding the state ('Rebuild State', 'Apply Events'), updating a separate data store represented by a cylinder labeled 'Historical State' (listing balances A:$5, B:$4, C:$3, D:$2). The 'Client' on the left can also issue a 'query' (indicated by a curved arrow) which is directed to a 'Query Service' within the 'Read Path'. The 'Query Service' accesses the 'Historical State' to fulfill client queries (arrow from 'Historical State' to 'Query Service'). The diagram illustrates the separation of command processing (writing) and query processing (reading), with events acting as the central source of truth to update both the primary state (State) and the read model (Historical State).
Figure 19 CQRS architecture
Candidate: In this design, we use event sourcing architecture to make the whole system reproducible. All valid business records are saved in an immutable Event queue which could be used for correctness verification.

Interviewer: That’s great. But the event sourcing architecture you proposed only handles one event at a time and it needs to communicate with several external systems. Can we make it faster?

Step 3 - Design Deep Dive
In this section, we dive deep into techniques for achieving high performance, reliability, and scalability.

High-performance event sourcing
In the earlier example, we used Kafka as the command and event store, and the database as a state store. Let’s explore some optimizations.

File-based command and event list
The first optimization is to save commands and events to a local disk, rather than to a remote store like Kafka. This avoids transit time across the network. The event list uses an append-only data structure. Appending is a sequential write operation, which is generally very fast. It works well even for magnetic hard drives because the operating system is heavily optimized for sequential reads and writes. According to this ACM Queue article [12], sequential disk access can be faster than random memory access in some cases.

The second optimization is to cache recent commands and events in memory. As we explained before, we process commands and events right after they are persisted. We may cache them in memory to save the time of loading them back from the local disk.

We are going to explore some implementation details. A technique called mmap [13] is great for implementing the optimizations mentioned previously. Mmap can write to a local disk and cache recent content in memory at the same time. It maps a disk file to memory as an array. The operating system caches certain sections of the file in memory to accelerate the read and write operations. For append-only file operations, it is almost guaranteed that all data are saved in memory, which is very fast.

Figure 20 shows the file-based command and event storage.

Image represents a system architecture diagram illustrating data flow between memory and disk storage.  The top section, labeled 'memory,' contains two rectangular boxes representing 'Command List' and 'Event List.'  A solid arrow points from 'Command List' to a circular element, likely representing a processing unit or transformation step, which then has a solid arrow pointing to 'Event List.'  The bottom section, labeled 'disk,' shows two file icons labeled 'Command File' and 'Event File.'  Dashed arrows labeled 'mmap' connect 'Command File' to 'Command List' and 'Event File' to 'Event List,' indicating that memory-mapped files are used to load data from disk into memory.  The overall flow suggests that commands are processed, resulting in events, with both commands and events being persistently stored in separate files on disk using memory mapping for efficient access.
Figure 20 File-based command and event storage
File-based state
In the previous design, state (balance information) is stored in a relational database. In a production environment, a database usually runs in a stand-alone server that can only be accessed through networks. Similar to the optimizations we did for command and event, state information can be saved to the local disk, as well.

More specifically, we can use SQLite [14], which is a file-based local relational database or use RocksDB [15], which is a local file-based key-value store.

RocksDB is chosen because it uses a log-structured merge-tree (LSM), which is optimized for write operations. To improve read performance, the most recent data is cached.

Figure 21 shows the file-based solution for command, event, and state.

Image represents a system architecture diagram illustrating data flow and persistence mechanisms.  The top section, labeled 'memory,' contains three key components: a 'Command List' rectangle, a circular processing element (likely representing a processor or event loop), and an 'Event List' rectangle.  Data flows unidirectionally from the 'Command List' to the processing element and then to the 'Event List.' Below the processing element is a database cylinder labeled 'State' and annotated as a 'RocksDB In-memory cache.'  Dashed lines indicate data flow between the 'Command List' and a 'Command File' located in the bottom 'disk' section, and similarly between the 'Event List' and an 'Event File' on the disk.  These connections are labeled 'mmap,' indicating memory mapping is used for persistent storage.  Finally, a dashed line connects the 'State' (in-memory cache) to a 'RocksDB File' on the disk, also using memory mapping ('mmap') for persistence.  The overall architecture shows a system processing commands, generating events, and persisting both commands, events, and the system's state to disk using memory mapping for efficient data access.
Figure 21 File-based solution for command, event, and state
Snapshot
Once everything is file-based, let us consider how to accelerate the reproducibility process. When we first introduced reproducibility, the state machine had to process events from the very beginning, every time. What we could optimize is to periodically stop the state machine and save the current state into a file. This is called a snapshot.

A snapshot is an immutable view of a historical state. Once a snapshot is saved, the state machine does not have to restart from the very beginning anymore. It can read data from a snapshot, verify where it left off, and resume processing from there.

For financial applications such as wallet service, the finance team often requires a snapshot to be taken at 00:00 so they can verify all transactions that happened during that day. When we first introduced CQRS of event sourcing, the solution was to set up a read-only state machine that reads from the beginning until the specified time is met. With snapshots, a read-only state machine only needs to load one snapshot that contains the data.

A snapshot is a giant binary file and a common solution is to save it in an object storage solution, such as HDFS [16].

Figure 22 shows the file-based event sourcing architecture. When everything is file-based, the system can fully utilize the maximum I/O throughput of the computer hardware.

Image represents a system architecture diagram illustrating a state machine replicated for read-only access.  The left side shows a primary state machine with a `Command List` feeding into a circular `Event List` buffer in memory.  This `Event List` is persistently stored as an `Event File` on disk, accessible via memory mapping (`mmap`).  A `RocksDB` in-memory cache sits between the `Event List` and the `RocksDB File` (also on disk and accessible via `mmap`), representing persistent state storage.  The `Command List` and `RocksDB` also have corresponding files on disk.  A dashed line indicates that the `Command List` and `RocksDB` are loaded into memory via `mmap`. The `Event List` is replicated to a second `Event List` on the right, forming a read-only state machine. This read-only machine has a circular buffer and is queried externally.  The bottom right shows an `Object Store` containing snapshots of the system's state, labeled with timestamps (e.g., '2020-03-03 24:00').  A dashed line indicates that snapshots are taken from the `Event File` and stored in the `Object Store`. The read-only state machine loads snapshots from the `Object Store` for efficient querying.
Figure 22 Snapshot
Candidate: We could refactor the design of event sourcing so the command list, event list, state, and snapshot are all saved in files. Event sourcing architecture processes the event list in a linear manner, which fits well into the design of hard disks and operating system cache.

Interviewer: The performance of the local file-based solution is better than the system that requires accessing data from remote Kafka and databases. However, there is another problem: because data is saved on a local disk, a server is now stateful and becomes a single point of failure. How do we improve the reliability of the system?

Reliable high-performance event sourcing
Before we explain the solution, let’s examine the parts of the system that need the reliability guarantee.

Reliability analysis
Conceptually, everything a node does is around two concepts; data and computation. As long as data is durable, it’s easy to recover the computational result by running the same code on another node. This means we only need to worry about the reliability of data because if data is lost, it is lost forever. The reliability of the system is mostly about the reliability of the data.

There are four types of data in our system.

File-based command

File-based event

File-based state

State snapshot

Let us take a close look at how to ensure the reliability of each type of data.

State and snapshot can always be regenerated by replaying the event list. To improve the reliability of state and snapshot, we just need to ensure the event list has strong reliability.

Now let us examine command. On the face of it, event is generated from command. We might think providing a strong reliability guarantee for command should be sufficient. This seems to be correct at first glance, but it misses something important. Event generation is not guaranteed to be deterministic, and also it may contain random factors such as random numbers, external I/O, etc. So command cannot guarantee reproducibility of events.

Now it’s time to take a close look at event. Event represents historical facts that introduce changes to the state (account balance.) Event is immutable and can be used to rebuild the state.

From this analysis, we conclude that event data is the only one that requires a high-reliability guarantee. We will explain how to achieve this in the next section.

Consensus
To provide high reliability, we need to replicate the event list across multiple nodes. During the replication process, we have to guarantee the following properties.

No data loss.

The relative order of data within a log file remains the same across nodes.

To achieve those guarantees, consensus-based replication is a good fit. The consensus algorithm makes sure that multiple nodes reach a consensus on what the event list is. Let’s use the Raft [17] consensus algorithm as an example.

The Raft algorithm guarantees that as long as more than half of the nodes are online, the append-only lists on them have the same data. For example, if we have 5 nodes and use the Raft algorithm to synchronize their data, as long as at least 3 (more than half) of the nodes are up as Figure 23 shows, the system can still work properly as a whole:

Image represents a simple diagram illustrating a concept likely related to system status or availability.  The diagram consists of five circular nodes arranged horizontally. The first three nodes are filled with light green color and labeled 'Up' below each circle, indicating an operational or active state. The last two nodes are unfilled (white) and labeled 'Down' below each circle, signifying an inactive or failed state.  There are no explicit connections drawn between the nodes; the arrangement simply shows a sequence or group of elements with differing statuses. The overall impression is a visual representation of a system's component health, where three components are up and two are down.
Figure 23 Raft
A node can have three different roles in the Raft algorithm.

Leader

Candidate

Follower

We can find the implementation of the Raft algorithm in the Raft paper. We will only cover the high level concepts here and not go into detail. In Raft, at most one node is the leader of the cluster and the remaining nodes are followers. The leader is responsible for receiving external commands and replicating data reliably across nodes in the cluster.

With the Raft algorithm, the system is reliable as long as the majority of the nodes are operational. For example, if there are 3 nodes in the cluster, it could tolerate the failure of 1 node, and if there are 5 nodes, it can tolerate the failure of 2 nodes.

Reliable solution
With replication, there won’t be a single point of failure in our file-based event sourcing architecture. Let’s take a look at the implementation details. Figure 24 shows the event sourcing architecture with the reliability guarantee.

Image represents a system architecture diagram illustrating a distributed database system using Raft consensus.  The diagram shows a Raft node group enclosed within a larger rectangle, composed of three horizontal layers. The top and bottom layers are light cyan, labeled 'Follower,' each containing an 'Events' box, a circular 'Raft' component representing the Raft consensus algorithm, and a database cylinder. The middle layer is light pink, labeled 'Commands,' and contains an 'Events' box, a circular 'Raft' component, and a database cylinder.  A user icon on the left sends commands via a solid arrow into the 'Commands' box in the middle layer.  The 'Commands' box processes these commands and sends them through its internal 'Raft' component to the 'Events' box, which then feeds into another circular 'Raft' component before finally writing to the database cylinder.  Dashed lines indicate communication between the Raft components across the three layers.  Outside the Raft node group, on the right, are two additional database cylinders, each preceded by a circular 'Raft' component.  These receive data from the internal Raft components via dashed lines. A user icon on the right interacts with the system via solid arrows labeled 'Query,' reading data from the external database cylinders. A horizontal arrow at the bottom labels the data flow as 'Write' from left to right and 'Read' from right to left, summarizing the overall data flow within the system.
Figure 24 Raft node group
In Figure 24, we set up 3 event sourcing nodes. These nodes use the Raft algorithm to synchronize the event list reliably.

The leader takes incoming command requests from external users, converts them into events, and appends events into the local event list. The Raft algorithm replicates newly added events to the followers.

All nodes, including the followers, process the event list and update the state. The Raft algorithm ensures the leader and followers have the same event lists, while event sourcing guarantees all states are the same, as long as the event lists are the same.

A reliable system needs to handle failures gracefully, so let’s explore how node crashes are handled.

If the leader crashes, the Raft algorithm automatically selects a new leader from the remaining healthy nodes. This newly elected leader takes responsibility for accepting commands from external users. It is guaranteed that the cluster as a whole can provide continued service when a node goes down.

When the leader crashes, it is possible that the crash happens before the command list is converted to events. In this case, the client would notice the issue either by a timeout or by receiving an error response. The client needs to resend the same command to the newly elected leader.

In contrast, follower crashes are much easier to handle. If a follower crashes, requests sent to it will fail. Raft handles failures by retrying indefinitely until the crashed node is restarted or a new one replaces it.

Candidate: In this design, we use the Raft consensus algorithm to replicate the event list across multiple nodes. The leader receives commands and replicates events to other nodes.

Interviewer: Yes, the system is more reliable and fault-tolerant. However, in order to handle 1 million TPS, one server is not enough. How can we make the system more scalable?

Distributed event sourcing
In the previous section, we explained how to implement a reliable high-performance event sourcing architecture. It solves the reliability issue, but it has two limitations.

When a digital wallet is updated, we want to receive the updated result immediately. But in the CQRS design, the request/response flow can be slow. This is because a client doesn’t know exactly when a digital wallet is updated and the client may need to rely on periodic polling.

The capacity of a single Raft group is limited. At a certain scale, we need to shard the data and implement distributed transactions.

Let’s take a look at how those two problems are solved.

Pull vs push
In the pull model, an external user periodically polls execution status from the read-only state machine. This model is not real-time and may overload the wallet service if the polling frequency is set too high. Figure 25 shows the pulling model.

Image represents a system architecture diagram illustrating a distributed system likely using the Raft consensus algorithm for data replication.  A user (represented by a person icon) sends commands via a red arrow to a central 'Commands' block (pink).  These commands are processed, generating 'Events' which flow through a circular buffer (represented by a gear-like icon) to a database within a Raft node group. This node group is depicted as three horizontal layers: a lower 'Follower' layer (light blue) which passively replicates data, a central 'Commands' and 'Events' layer (pink) which processes commands and generates events, and an upper 'Follower' layer (light blue) mirroring the lower one.  Each layer contains an 'Events' block, a circular buffer, and a database.  Data flows between these components using solid arrows.  The 'Raft' labels indicate the Raft consensus protocol managing data consistency across the nodes.  Dashed black arrows show data replication between the Raft node group and external follower nodes (represented by circular buffers and databases to the right).  A dashed red arrow shows data being read from the central Raft node group.  A light blue arrow at the top shows a periodic query for the latest status, suggesting a mechanism for monitoring or consistency checks.  Finally, a horizontal arrow labeled 'Write' indicates the direction of write operations, and another labeled 'Read' shows the direction of read operations, highlighting the data flow within the system.
Figure 25 Periodical pulling
The naive pull model can be improved by adding a reverse proxy [18] between the external user and the event sourcing node. In this design, the external user sends a command to the reverse proxy, which forwards the command to event sourcing nodes and periodically polls the execution status. This design simplifies the client logic, but the communication is still not real-time.

Figure 26 shows the pull model with a reverse proxy added.

Image represents a system architecture diagram illustrating a distributed system likely using Raft consensus for data replication.  A user interacts with a 'Reverse Proxy' component, sending 'Command' requests (red arrow) which are processed.  The core system consists of three horizontally stacked Raft node groups, each containing a 'Follower', 'Events' processing unit, a 'Raft' consensus component represented as a gear, and a data store (database symbol). The middle group is highlighted in pink and handles 'Commands', while the top and bottom groups (light blue) are 'Followers' that replicate data.  Commands flow (red arrow) through the central Raft group, generating 'Events' that are then replicated to the other two groups via the Raft consensus mechanism (dashed black lines).  Data is read (blue arrow) from any of the three database instances.  A light blue line indicates a periodic query from the system to retrieve the latest status.  A horizontal black arrow labeled 'Write' indicates the direction of command processing, and a horizontal black arrow labeled 'Read' shows the direction of data retrieval.  The entire system is designed for high availability and fault tolerance through the use of Raft.
Figure 26 Pull model with reverse proxy
Once we have the reverse proxy, we could make the response faster by modifying the read-only state machine. As we mentioned earlier, the read-only state machine could have its own behavior. For example, one behavior could be that the read-only state machine pushes execution status back to the reverse proxy, as soon as it receives the event. This will give the user a feeling of real-time response.

Figure 27 shows the push-based model.

Image represents a system architecture diagram illustrating a distributed system using Raft consensus for data replication.  A user interacts with a reverse proxy, sending commands (indicated by a red curved arrow). The reverse proxy forwards these commands to a central 'Commands' component (pink rectangle) within a Raft node group. This component processes the commands, generating events which are then processed through a Raft consensus mechanism (indicated by the 'Raft' labels and arrows).  The events flow through an 'Events' component (within the pink rectangle) to a replicated database (represented by cylinders).  Two additional Raft follower nodes (light blue rectangles) mirror this process, receiving events and updating their own databases.  Data consistency is maintained through the Raft protocol.  The system also supports read operations (indicated by a black dashed arrow), allowing external access to the replicated data.  Finally, a light blue arrow shows the system pushing the latest status from the replicated database to the reverse proxy in real-time, enabling the reverse proxy to provide up-to-date responses to the user (indicated by a blue curved arrow).  The entire Raft node group is labeled 'Raft Node Group,' and the horizontal arrows at the bottom indicate the direction of write and read operations.
Figure 27 Push model
Distributed transaction
Once synchronous execution is adopted for every event sourcing node group, we can reuse the distributed transaction solution, TC/C or Saga. Assume we partition the data by dividing the hash value of keys by 2.

Figure 28 shows the updated design.

Image represents a system architecture diagram illustrating a distributed system using Raft for consensus and handling commands and events.  The system is partitioned into two parts, 'Partition 1 (account A)' and 'Partition 2 (account C),' each containing a Raft node group. Each Raft group consists of three nodes: two followers and one leader (indicated by the double-circle).  Within each partition, the Raft group processes commands (shown flowing from left to right in a pink rectangle) and events (in light blue rectangles).  Commands are processed by the leader node, and events are replicated across all nodes.  A 'TCC/Saga Coordinator' interacts with the system, sending commands (red arrows) and receiving responses (blue arrows).  The coordinator also maintains a 'Phase Status Table.'  A 'Reverse Proxy' acts as an intermediary, receiving real-time status updates (light blue arrows) pushed from each partition's Raft node group.  Dashed lines indicate data flow within the Raft group, while solid arrows show interactions between components.  The system is designed for high availability and fault tolerance through the use of Raft's consensus mechanism.
Figure 28 Final design
Let’s take a look at how the money transfer works in the final distributed event sourcing architecture. To make it easier to understand, we use the Saga distributed transaction model and only explain the happy path without any rollback.

The money transfer operation contains 2 distributed operations: A-$1 and C+$1. The Saga coordinator coordinates the execution as shown in Figure 29:

User A sends a distributed transaction to the Saga coordinator. It contains two operations: A-$1 and C+$1.

Saga coordinator creates a record in the phase status table to trace the status of a transaction.

Saga coordinator examines the order of operations and determines that it needs to handle A-$1 first. The coordinator sends A-$1 as a command to Partition 1, which contains account A’s information.

Partition 1’s Raft leader receives the A-$1 command and stores it in the command list. It then validates the command. If it is valid, it is converted into an event. The Raft consensus algorithm is used to synchronize data across different nodes. The event (deducting $1 from A’s account balance) is executed after synchronization is complete.

After the event is synchronized, the event sourcing framework of Partition 1 synchronizes the data to the read path using CQRS. The read path reconstructs the state and the status of execution.

The read path of Partition 1 pushes the status back to the caller of the event sourcing framework, which is the Saga coordinator.

Saga coordinator receives the success status from Partition 1.

The Saga coordinator creates a record, indicating the operation in Partition 1 is successful, in the phase status table.

Because the first operation succeeds, the Saga coordinator executes the second operation, which is C+$1. The coordinator sends C+$1 as a command to Partition 2 which contains account C’s information.

Partition 2’s Raft leader receives the C+$1 command and saves it to the command list. If it is valid, it is converted into an event. The Raft consensus algorithm is used to synchronize data across different nodes. The event (add $1 to C’s account ) is executed after synchronization is complete.

After the event is synchronized, the event sourcing framework of Partition 2 synchronizes the data to the read path using CQRS. The read path reconstructs the state and the status of execution.

The read path of Partition 2 pushes the status back to the caller of the event sourcing framework, which is the Saga coordinator.

The Saga coordinator receives the success status from Partition 2.

The Saga coordinator creates a record, indicating the operation in Partition 2 is successful in the phase status table.

At this time, all operations succeed and the distributed transaction is completed. The Saga coordinator responds to its caller with the result.

Image represents a system architecture diagram illustrating a distributed transaction processing system using TCC/Saga and Raft consensus.  The diagram shows two partitions, Partition 1 (account A) and Partition 2 (account C), each containing a Raft Node Group. Each Raft group consists of three nodes: two followers and one leader (indicated by the circular shape with a bold outline).  Each node in the Raft group has 'Events' and 'Commands' components.  A 'TCC/Saga Coordinator' (1) interacts with each partition via a 'Reverse Proxy'.  Commands (3, 9) are sent from the coordinator to the 'Commands' component of the leader node in each partition's Raft group (4, 10).  Events (4, 5, 10, 11) are generated and propagated within each Raft group.  The 'Events' component of the leader node in each partition pushes the latest status to the reverse proxy in real-time (6, 12).  The reverse proxy receives responses (7, 13) from the partitions.  A 'Phase Status Table' (14) is used by the coordinator.  The numbers (1-15) label the components and data flows.  Dashed lines represent asynchronous communication, while solid lines represent synchronous communication.  The system uses a write-then-read pattern, with writes occurring within the Raft group and reads occurring from the database (represented by cylindrical shapes) after successful replication.
Figure 29 Final design in a numbered sequence
Step 4 - Wrap Up
In this chapter, we designed a wallet service that is capable of processing over 1 million payment commands per second. After a back-of-the-envelope estimation, we concluded that a few thousand nodes are required to support such a load.

In the first design, a solution using in-memory key-value stores like Redis is proposed. The problem with this design is that data isn't durable.

In the second design, the in-memory cache is replaced by transactional databases. To support multiple nodes, different transactional protocols such as 2PC, TC/C, and Saga are proposed. The main issue with transaction-based solutions is that we cannot conduct a data audit easily.

Next, event sourcing is introduced. We first implemented event sourcing using an external database and queue, but it’s not performant. We improved performance by storing command, event, and state in a local node.

A single node means a single point of failure. To increase the system reliability, we use the Raft consensus algorithm to replicate the event list onto multiple nodes.

The last enhancement we made was to adopt the CQRS feature of event sourcing. We added a reverse proxy to change the asynchronous event sourcing framework to a synchronous one for external users. The TC/C or Saga protocol is used to coordinate Command executions across multiple node groups.

Congratulations on getting this far! Now give yourself a pat on the back. Good job!

Chapter Summary
Image represents a system design process for a Digital Wallet, broken down into four steps.  The process begins with defining requirements: 'functional req' specifies the core functionality – 'money transfer between two accounts' with a target of '1 million TPS' – while 'non-functional req' outlines performance goals ('reliability 99.99%'), transaction support, and reproducibility. Step 1 connects these requirements to the overall design. Step 2 focuses on the API design ('wallet/balance_transfer') and database solutions, exploring options like 'in-memory sharding solution', 'database sharding', '2 phase commit', 'try-confirm cancel', and handling 'out-of-order execution' using 'event sourcing'.  Step 3 delves into different event sourcing approaches, including 'high-performance event sourcing', 'reliable event sourcing', and 'distributed event sourcing', branching into considerations of 'pull vs push' and 'distributed transaction' mechanisms. Finally, step 4 concludes with a 'wrap up' phase, suggesting the completion of the design process.  The entire diagram is a tree-like structure, with 'Digital Wallet' at the root, and each step branching into various design choices and considerations.
Reference materials
[1] Transactional guarantees:
https://docs.oracle.com/cd/E17275_01/html/programmer_reference/rep_trans.html

[2] TPC-E Top Price/Performance Results:
http://tpc.org/tpce/results/tpce_price_perf_results5.asp?resulttype=all

[3] ISO 4217 CURRENCY CODES: https://en.wikipedia.org/wiki/ISO_4217

[4] Apache Zookeeper: https://zookeeper.apache.org/

[5] Martin Kleppmann (2017). Designing Data-Intensive Applications. O'Reilly Media.

[6] X/Open XA: https://en.wikipedia.org/wiki/X/Open_XA

[7] Compensating transaction: https://en.wikipedia.org/wiki/Compensating_transaction

[8] SAGAS, HectorGarcia-Molina: https://www.cs.cornell.edu/andru/cs711/2002fa/reading/sagas.pdf

[9] Evans, E. (2003). Domain-Driven Design: Tackling Complexity in the Heart of Software. Addison-Wesley Professional.

[10] Apache Kafka. https://kafka.apache.org/

[11] CQRS: https://martinfowler.com/bliki/CQRS.html

[12] Comparing Random and Sequential Access in Disk and Memory:
https://deliveryimages.acm.org/10.1145/1570000/1563874/jacobs3.jpg

[13] mmap: https://man7.org/linux/man-pages/man2/mmap.2.html

[14] SQLite: https://www.sqlite.org/index.html

[15] RocksDB. https://rocksdb.org/

[16] Apache Hadoop: https://hadoop.apache.org/

[17] Raft: https://raft.github.io/

[18] Reverse proxy: https://en.wikipedia.org/wiki/Reverse_proxy


Mark as Complete
Partner With Us
Teach on ByteByteGo
Be an Affiliate
Become a Contributor
Support
hi@bytebytego.com
Report a Bug
Company & Legal
Our Team
Newsletter
Privacy Policy
Terms of Service
Resources
YouTube
Visual Dev Guides
Find Jobs
Prepare for Coding Interviews
Copyright ©2022-2026 ByteByteGo Inc. All rights reserved.

ask alexask alex expend
ByteByteGo | Technical Interview Prep